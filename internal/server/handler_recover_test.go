package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llmgate/internal/llmrouter"
	"llmgate/internal/llmtypes"
	"llmgate/internal/platform/http/response"
	"llmgate/internal/telemetry"
)

// Panic paths must stamp audit as panic/500 and keep panic values out
// of the wire response.
func TestHandler_PanicInComplete_StampsAuditAndReturns500(t *testing.T) {
	rec, recorder := newCaptureAuditSink()
	svc := &fakeService{
		vendor: "opencode",
		buildResult: func(req *llmtypes.Request) *llmrouter.RouteResult {
			panic("boom in complete")
		},
	}
	h := NewHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil)), recorder, HandlerConfig{})

	body := `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	got := rec.last(t)
	if got.Kind != llmtypes.KindPanic {
		t.Errorf("Kind = %q, want %q", got.Kind, llmtypes.KindPanic)
	}
	if got.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want 500", got.StatusCode)
	}
	if strings.Contains(w.Body.String(), "boom") {
		t.Errorf("wire body leaked panic value: %s", w.Body.String())
	}
}

func TestHandler_PanicInStream_StampsAuditAndReturns500(t *testing.T) {
	rec, recorder := newCaptureAuditSink()
	svc := &fakeService{
		vendor: "opencode",
		buildStreamResult: func(req *llmtypes.Request) (*llmrouter.RouteResult, error) {
			panic("boom in stream")
		},
	}
	h := NewHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil)), recorder, HandlerConfig{})

	body := `{"model":"deepseek-v4-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	got := rec.last(t)
	if got.Kind != llmtypes.KindPanic {
		t.Errorf("Kind = %q, want %q", got.Kind, llmtypes.KindPanic)
	}
	if got.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want 500", got.StatusCode)
	}
	if strings.Contains(w.Body.String(), "boom") {
		t.Errorf("wire body leaked panic value: %s", w.Body.String())
	}
}

// Once the response has started, panic recovery must only update audit;
// writing a JSON body would corrupt the in-flight SSE stream.
func TestHandler_PanicAfterResponseStarted_DoesNotCorruptWireBody(t *testing.T) {
	rec, recorder := newCaptureAuditSink()
	svc := &fakeService{
		vendor: "opencode",
		buildResult: func(req *llmtypes.Request) *llmrouter.RouteResult {
			panic("late boom")
		},
	}
	h := NewHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil)), recorder, HandlerConfig{})

	body := `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))

	inner := httptest.NewRecorder()
	// Simulate streamRelay having already flushed 200/SSE headers.
	cw := response.NewCountingWriter(inner)
	cw.WriteHeader(http.StatusOK)

	h.ServeHTTP(cw, req)

	got := rec.last(t)
	if got.Kind != llmtypes.KindPanic {
		t.Errorf("Kind = %q, want %q", got.Kind, llmtypes.KindPanic)
	}
	if got.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want 500", got.StatusCode)
	}

	if inner.Body.Len() != 0 {
		t.Errorf("wire body = %q, want empty (mid-stream panic must not write body)", inner.Body.String())
	}
}

// http.ErrAbortHandler is an intentional abort sentinel, not a panic
// outcome to stamp into audit.
func TestHandler_AbortHandlerPanic_Repropagates_NotStamped(t *testing.T) {
	rec, recorder := newCaptureAuditSink()
	svc := &fakeService{
		vendor: "opencode",
		buildResult: func(req *llmtypes.Request) *llmrouter.RouteResult {
			panic(http.ErrAbortHandler)
		},
	}
	h := NewHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil)), recorder, HandlerConfig{})

	body := `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()

	func() {
		defer func() {
			r := recover()
			if r != http.ErrAbortHandler {
				t.Fatalf("recovered = %v, want http.ErrAbortHandler propagated upward", r)
			}
		}()
		h.ServeHTTP(w, req)
	}()

	got := rec.last(t)
	if got.Kind == llmtypes.KindPanic {
		t.Errorf("Kind = %q, want non-panic for intentional http.ErrAbortHandler abort", got.Kind)
	}
}

// Panic audit status records the outcome, not any earlier wire status.
func TestHandler_RecoverPanic_OverridesPreStampedStatus(t *testing.T) {
	_, recorder := newCaptureAuditSink()
	h := NewHandler(
		&fakeService{vendor: "opencode"},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		recorder,
		HandlerConfig{},
	)

	rec := &telemetry.AuditEvent{
		EventCommon: telemetry.EventCommon{
			RequestID:  "test-req-id",
			StatusCode: http.StatusOK,
		},
	}
	inner := httptest.NewRecorder()
	cw := response.NewCountingWriter(inner)
	cw.WriteHeader(http.StatusOK)

	h.recoverPanic(context.Background(), cw, rec, "mid-stream boom")

	if rec.Kind != llmtypes.KindPanic {
		t.Errorf("Kind = %q, want %q", rec.Kind, llmtypes.KindPanic)
	}
	if rec.StatusCode != http.StatusInternalServerError {
		t.Errorf(
			"StatusCode = %d, want 500; panic must override pre-stamped 200",
			rec.StatusCode,
		)
	}
	if inner.Body.Len() != 0 {
		t.Errorf("wire body = %q, want empty (already-flushed response must not get JSON written)", inner.Body.String())
	}
}

func TestHandler_TelemetrySinkPanic_DoesNotBreakResponse(t *testing.T) {
	h := NewHandler(okFakeService(), slog.New(slog.NewTextHandler(io.Discard, nil)), panicEventSink{}, HandlerConfig{})

	body := `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
}

func TestHandler_LifecyclePanic_DoesNotBreakResponse(t *testing.T) {
	h := NewHandler(okFakeService(), slog.New(slog.NewTextHandler(io.Discard, nil)), telemetry.NopSink{}, HandlerConfig{
		LifecycleObserver: panicLifecycleObserver{},
	})

	body := `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
}
