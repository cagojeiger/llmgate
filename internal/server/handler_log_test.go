package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llmgate/internal/domain/routing"
	"llmgate/internal/llmtypes"
	"llmgate/internal/providers/fake"
	"llmgate/internal/telemetry"
)

func TestHandler_LogContract_AuthFailure(t *testing.T) {
	auditBuf, callBuf, sink := newLogContractSink()
	h := NewHandler(okFakeService(), slog.New(slog.NewTextHandler(io.Discard, nil)), sink, HandlerConfig{})

	body := `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req = requestWithTelemetryContext(req, "req-auth-contract", &ConsumerInfo{AuthError: telemetry.AuthErrorMissing})
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", w.Code, w.Body.String())
	}
	audit := decodeSingleLogLine(t, auditBuf)
	wantLogField(t, audit, "log", "audit")
	wantLogField(t, audit, "event_type", "audit")
	wantLogField(t, audit, "request_id", "req-auth-contract")
	wantLogNumber(t, audit, "status", http.StatusUnauthorized)
	wantLogField(t, audit, "operation", "chat.completions")
	wantLogField(t, audit, "auth_result", "failure")
	wantLogField(t, audit, "auth_error", "missing")
	wantLogField(t, audit, "policy_result", "denied")
	wantLogField(t, audit, "deny_reason", "auth")
	wantLogField(t, audit, "error_kind", "auth")
	if callBuf.Len() != 0 {
		t.Fatalf("auth failure must not emit call log, got %s", callBuf.String())
	}
	assertLogDoesNotContainSensitiveMaterial(t, auditBuf, callBuf)
}

func TestHandler_LogContract_NonStreamSuccess(t *testing.T) {
	auditBuf, callBuf, sink := newLogContractSink()
	h := NewHandler(okFakeService(), slog.New(slog.NewTextHandler(io.Discard, nil)), sink, HandlerConfig{})

	body := `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"Reply with exactly OK."}],"max_tokens":8}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer example-key-001")
	req = requestWithTelemetryContext(req, "req-non-stream-contract", &ConsumerInfo{
		Name:  "example",
		KeyID: "467d813a",
	})
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	audit := decodeSingleLogLine(t, auditBuf)
	call := decodeSingleLogLine(t, callBuf)
	assertSuccessAuditLog(t, audit, "req-non-stream-contract", "chat.completions")
	assertSuccessCallLog(t, call, "req-non-stream-contract", "chat.completions")
	wantLogNumber(t, call, "final_attempt_status", http.StatusOK)
	assertLogDoesNotContainSensitiveMaterial(t, auditBuf, callBuf)
}

func TestHandler_LogContract_StreamSuccess(t *testing.T) {
	auditBuf, callBuf, sink := newLogContractSink()
	streamObj := fake.NewStream(
		fake.WithEvents([]*llmtypes.Event{
			{Choices: []llmtypes.ChoiceDelta{{Delta: llmtypes.Delta{Content: "OK"}}}},
		}),
		fake.WithSummary(&llmtypes.Summary{
			Usage: &llmtypes.Usage{PromptTokens: 9, CompletionTokens: 2, TotalTokens: 11},
		}),
	)
	svc := &fakeService{
		buildStreamResult: func(req *llmtypes.Request) (*routing.RouteResult, error) {
			return &routing.RouteResult{
				Stream:    streamObj,
				Vendor:    "opencode",
				ModelUsed: req.Model,
				Attempts: []llmtypes.Attempt{
					{Vendor: "opencode", Model: req.Model, StartedAt: time.Now()},
				},
			}, nil
		},
	}
	h := NewHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil)), sink, HandlerConfig{})

	body := strings.Join([]string{
		`{"model":"deepseek-v4-flash","stream":true,`,
		`"messages":[{"role":"user","content":"Reply with exactly OK."}],`,
		`"max_tokens":8}`,
	}, "")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer example-key-001")
	req = requestWithTelemetryContext(req, "req-stream-contract", &ConsumerInfo{
		Name:  "example",
		KeyID: "467d813a",
	})
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	audit := decodeSingleLogLine(t, auditBuf)
	call := decodeSingleLogLine(t, callBuf)
	assertSuccessAuditLog(t, audit, "req-stream-contract", "chat.completions.stream")
	assertSuccessCallLog(t, call, "req-stream-contract", "chat.completions.stream")
	wantLogNumber(t, call, "final_attempt_status", http.StatusOK)
	wantLogNumber(t, call, "prompt_tokens", 9)
	wantLogNumber(t, call, "completion_tokens", 2)
	wantLogNumber(t, call, "total_tokens", 11)
	assertLogDoesNotContainSensitiveMaterial(t, auditBuf, callBuf)
}
