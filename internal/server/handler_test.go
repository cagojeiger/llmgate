package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"llmgate/internal/llmrouter"
	"llmgate/internal/llmtypes"
	"llmgate/internal/providers/fake"
	"llmgate/internal/telemetry"
)

func TestHandler_SingleAttempt_RecordPopulated(t *testing.T) {
	rec, recorder := newCaptureAuditSink()
	callRec, callSink := newCaptureCallSink()
	r := &fakeService{
		vendor: "opencode",
		buildResult: func(req *llmtypes.Request) *llmrouter.RouteResult {
			return &llmrouter.RouteResult{
				Response: &llmtypes.Response{
					Model:   req.Model,
					Choices: []llmtypes.Choice{{Index: 0, Message: llmtypes.Message{Role: "assistant", Content: "ok"}}},
				},
				Vendor:    "opencode",
				ModelUsed: req.Model,
				Attempts: []llmtypes.Attempt{
					{Vendor: "opencode", Model: req.Model, StatusCode: 200, StartedAt: time.Now()},
				},
			}
		},
	}
	h := NewHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)), telemetry.NewFanoutSink(nil, recorder, callSink), HandlerConfig{})

	body := `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	gotAudit := rec.last(t)
	if gotAudit.StatusCode != http.StatusOK {
		t.Errorf("audit StatusCode = %d, want 200", gotAudit.StatusCode)
	}
	if gotAudit.AuthResult != telemetry.AuthResultSuccess || gotAudit.PolicyResult != telemetry.PolicyResultAllowed {
		t.Errorf("audit decisions = auth:%q policy:%q, want success/allowed", gotAudit.AuthResult, gotAudit.PolicyResult)
	}
	if gotAudit.ResourceType != "llm_model" || gotAudit.ResourceID != "deepseek-v4-flash" {
		t.Errorf("audit resource = %q/%q, want llm_model/deepseek-v4-flash", gotAudit.ResourceType, gotAudit.ResourceID)
	}
	got := callRec.last(t)
	if got.ModelRequested != "deepseek-v4-flash" {
		t.Errorf("ModelRequested = %q, want deepseek-v4-flash", got.ModelRequested)
	}
	if got.Vendor != "opencode" || got.ModelUsed != "deepseek-v4-flash" {
		t.Errorf("Vendor/ModelUsed = %q/%q, want opencode/deepseek-v4-flash", got.Vendor, got.ModelUsed)
	}
	if len(got.Attempts) != 1 {
		t.Fatalf("len(Attempts) = %d, want 1", len(got.Attempts))
	}
	if got.Attempts[0].Vendor != "opencode" || got.Attempts[0].Model != "deepseek-v4-flash" {
		t.Errorf("attempt = %+v, want opencode/deepseek-v4-flash", got.Attempts[0])
	}
}

func TestHandler_FallbackChain_AttemptsRecorded(t *testing.T) {
	_, recorder := newCaptureAuditSink()
	callRec, callSink := newCaptureCallSink()
	r := &fakeService{
		vendor: "opencode",
		buildResult: func(req *llmtypes.Request) *llmrouter.RouteResult {
			return &llmrouter.RouteResult{
				Response: &llmtypes.Response{
					Model:   "deepseek-v4-flash",
					Choices: []llmtypes.Choice{{Index: 0, Message: llmtypes.Message{Role: "assistant", Content: "ok"}}},
				},
				Vendor:    "opencode",
				ModelUsed: "deepseek-v4-flash",
				Attempts: []llmtypes.Attempt{
					{Vendor: "opencode", Model: "deepseek-v4-pro", Kind: llmtypes.KindRateLimit, StatusCode: 429, StartedAt: time.Now()},
					{Vendor: "opencode", Model: "deepseek-v4-flash", StatusCode: 200, StartedAt: time.Now()},
				},
			}
		},
	}
	h := NewHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)), telemetry.NewFanoutSink(nil, recorder, callSink), HandlerConfig{})

	body := `{"model":"coder","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	got := callRec.last(t)
	if got.ModelRequested != "coder" {
		t.Errorf("ModelRequested = %q, want coder (alias)", got.ModelRequested)
	}
	if got.ModelUsed != "deepseek-v4-flash" {
		t.Errorf("ModelUsed = %q, want deepseek-v4-flash (last attempt)", got.ModelUsed)
	}
	if len(got.Attempts) != 2 {
		t.Fatalf("len(Attempts) = %d, want 2", len(got.Attempts))
	}
	if got.Attempts[0].Kind != llmtypes.KindRateLimit {
		t.Errorf("attempts[0].Kind = %q, want rate_limit", got.Attempts[0].Kind)
	}
}

func TestAdoptError_ProviderErrorMapsKindAndStatus(t *testing.T) {
	cases := []struct {
		name       string
		kind       llmtypes.ErrorKind
		wantStatus int
	}{
		{"auth", llmtypes.KindAuth, http.StatusUnauthorized},
		{"forbidden", llmtypes.KindForbidden, http.StatusForbidden},
		{"rate_limit", llmtypes.KindRateLimit, http.StatusTooManyRequests},
		{"bad_request", llmtypes.KindBadRequest, http.StatusBadRequest},
		{"context_length", llmtypes.KindContextLength, http.StatusBadRequest},
		{"upstream", llmtypes.KindUpstream, http.StatusBadGateway},
		{"timeout", llmtypes.KindTimeout, http.StatusBadGateway},
		{"unknown", llmtypes.KindUnknown, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &telemetry.AuditEvent{}
			adoptError(rec, &llmtypes.Error{Kind: tc.kind, Message: "x"})
			if rec.Kind != tc.kind {
				t.Errorf("Kind = %q, want %q", rec.Kind, tc.kind)
			}
			if rec.StatusCode != tc.wantStatus {
				t.Errorf("StatusCode = %d, want %d", rec.StatusCode, tc.wantStatus)
			}
		})
	}
}

func TestHandler_AllowedAliasesRejectBeforeService(t *testing.T) {
	rec, recorder := newCaptureAuditSink()
	callRec, callSink := newCaptureCallSink()
	serviceCalled := false
	r := &fakeService{
		buildResult: func(req *llmtypes.Request) *llmrouter.RouteResult {
			serviceCalled = true
			return &llmrouter.RouteResult{
				Response: &llmtypes.Response{Model: req.Model},
				Vendor:   "opencode",
			}
		},
	}
	h := NewHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)), telemetry.NewFanoutSink(nil, recorder, callSink), HandlerConfig{})

	body := `{"model":"smart","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), consumerCtxKey{}, &ConsumerInfo{
		Name:           "alpha",
		KeyID:          "12345678",
		AllowedAliases: []string{"cheap"},
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", w.Code, w.Body.String())
	}
	if serviceCalled {
		t.Fatal("service called for disallowed model")
	}
	got := rec.last(t)
	if got.ConsumerName != "alpha" || got.ConsumerKeyID != "12345678" {
		t.Fatalf("consumer audit = %q/%q, want alpha/12345678", got.ConsumerName, got.ConsumerKeyID)
	}
	if got.Kind != llmtypes.KindForbidden || got.StatusCode != http.StatusForbidden {
		t.Fatalf("Kind/StatusCode = %q/%d, want forbidden/403", got.Kind, got.StatusCode)
	}
	if callRec.len() != 0 {
		t.Fatalf("call records = %d, want 0 for allowlist rejection", callRec.len())
	}
}

func TestAdoptError_NonProviderError_Falls500Unknown(t *testing.T) {
	rec := &telemetry.AuditEvent{}
	adoptError(rec, io.ErrUnexpectedEOF)
	if rec.Kind != llmtypes.KindUnknown {
		t.Errorf("Kind = %q, want unknown", rec.Kind)
	}
	if rec.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want 500", rec.StatusCode)
	}
}

func TestAdoptStreamSummary_FinalizesAttemptAndRecord(t *testing.T) {
	started := time.Unix(1700000000, 0)
	now := started.Add(250 * time.Millisecond)
	call := &telemetry.CallEvent{
		EventCommon: telemetry.EventCommon{StatusCode: http.StatusOK},
		Attempts: []llmtypes.Attempt{
			{Vendor: "v", Model: "m", StartedAt: started},
		},
	}
	sum := &llmtypes.Summary{
		Usage:      &llmtypes.Usage{PromptTokens: 5, CompletionTokens: 7, TotalTokens: 12},
		VendorCost: `"0.001"`,
	}

	telemetry.AdoptStreamSummary(call, sum, now)

	if call.Usage == nil || call.Usage.TotalTokens != 12 {
		t.Errorf("call.Usage = %+v, want total=12", call.Usage)
	}
	if call.VendorCost != `"0.001"` {
		t.Errorf("call.VendorCost = %q, want \"0.001\"", call.VendorCost)
	}
	last := call.Attempts[0]
	if last.DurationMS != 250 {
		t.Errorf("last.DurationMS = %d, want 250", last.DurationMS)
	}
	if last.StatusCode != http.StatusOK {
		t.Errorf("last.StatusCode = %d, want 200 propagated", last.StatusCode)
	}
	if last.Usage == nil || last.Usage.TotalTokens != 12 {
		t.Errorf("last.Usage = %+v, want total=12 propagated", last.Usage)
	}
	if last.VendorCost != `"0.001"` {
		t.Errorf("last.VendorCost = %q, want \"0.001\" propagated", last.VendorCost)
	}
}

func TestAdoptStreamSummary_PropagatesRecvErrorKindToAttempt(t *testing.T) {
	// Recv loop set rec.Kind; the deferred summary sync must mirror
	// it onto the in-flight Attempt so audit logs stay symmetric with the
	// non-stream path.
	started := time.Unix(1700000000, 0)
	now := started.Add(100 * time.Millisecond)
	call := &telemetry.CallEvent{
		EventCommon: telemetry.EventCommon{Kind: llmtypes.KindUpstream},
		Attempts: []llmtypes.Attempt{
			{Vendor: "v", Model: "m", StartedAt: started},
		},
	}

	telemetry.AdoptStreamSummary(call, nil, now)

	if call.Attempts[0].Kind != llmtypes.KindUpstream {
		t.Errorf("attempt ErrorKind = %q, want upstream", call.Attempts[0].Kind)
	}
	if call.Attempts[0].DurationMS != 100 {
		t.Errorf("DurationMS = %d, want 100", call.Attempts[0].DurationMS)
	}
}

func TestHandler_Stream_NormalEOF(t *testing.T) {
	captured, recorder := newCaptureAuditSink()
	callCaptured, callSink := newCaptureCallSink()
	lifecycle := &captureLifecycle{}
	streamObj := fake.NewStream(
		fake.WithEvents([]*llmtypes.Event{
			{Choices: []llmtypes.ChoiceDelta{{Delta: llmtypes.Delta{Content: "hello"}}}},
			{Choices: []llmtypes.ChoiceDelta{{Delta: llmtypes.Delta{Content: " world"}}}},
		}),
		fake.WithSummary(&llmtypes.Summary{
			Usage: &llmtypes.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5},
		}),
	)
	r := &fakeService{
		buildStreamResult: func(req *llmtypes.Request) (*llmrouter.RouteResult, error) {
			return &llmrouter.RouteResult{
				Stream:    streamObj,
				Vendor:    "opencode",
				ModelUsed: req.Model,
				Attempts: []llmtypes.Attempt{
					{Vendor: "opencode", Model: req.Model, StartedAt: time.Now()},
				},
			}, nil
		},
	}
	h := NewHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)), telemetry.NewFanoutSink(nil, recorder, callSink), HandlerConfig{
		LifecycleObserver: lifecycle,
	})

	body := `{"model":"deepseek-v4-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if !strings.Contains(w.Body.String(), `"hello"`) || !strings.Contains(w.Body.String(), `" world"`) {
		t.Errorf("body missing chunks: %q", w.Body.String())
	}
	if !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
		t.Errorf("body must end with [DONE] frame: %q", w.Body.String())
	}
	if got := streamObj.Closed(); got != 1 {
		t.Errorf("Stream.Close() calls = %d, want 1", got)
	}

	got := captured.last(t)
	if got.Operation != "chat.completions.stream" {
		t.Errorf("Operation = %q, want chat.completions.stream", got.Operation)
	}
	if got.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", got.StatusCode)
	}
	gotCall := callCaptured.last(t)
	if gotCall.Usage == nil || gotCall.Usage.TotalTokens != 5 {
		t.Errorf("Usage = %+v, want total=5 from Summary", gotCall.Usage)
	}
	if len(gotCall.Attempts) != 1 {
		t.Fatalf("Attempts = %d, want 1", len(gotCall.Attempts))
	}
	if gotCall.Attempts[0].Usage == nil || gotCall.Attempts[0].Usage.TotalTokens != 5 {
		t.Errorf("Attempts[0].Usage not finalized from Summary: %+v", gotCall.Attempts[0].Usage)
	}
	if gotCall.Attempts[0].StatusCode != http.StatusOK {
		t.Errorf("Attempts[0].StatusCode = %d, want 200", gotCall.Attempts[0].StatusCode)
	}
	if gotCall.ResponseBytes <= 0 {
		t.Errorf("ResponseBytes = %d, want > 0", gotCall.ResponseBytes)
	}
	if lifecycle.requestStarted != 1 || lifecycle.requestFinished != 1 {
		t.Errorf("request lifecycle = started:%d finished:%d, want 1/1", lifecycle.requestStarted, lifecycle.requestFinished)
	}
	if lifecycle.streamStarted != 1 || lifecycle.streamFinished != 1 {
		t.Errorf("stream lifecycle = started:%d finished:%d, want 1/1", lifecycle.streamStarted, lifecycle.streamFinished)
	}
	if lifecycle.streamCommon.Operation != "chat.completions.stream" {
		t.Errorf("stream lifecycle operation = %q, want chat.completions.stream", lifecycle.streamCommon.Operation)
	}
	if lifecycle.streamAudit == nil || lifecycle.streamAudit.StatusCode != http.StatusOK {
		t.Fatalf("stream lifecycle audit = %+v, want status 200", lifecycle.streamAudit)
	}
	if lifecycle.streamCall == nil || lifecycle.streamCall.Usage == nil || lifecycle.streamCall.Usage.TotalTokens != 5 {
		t.Fatalf("stream lifecycle call usage = %+v, want summary tokens before finish notification", lifecycle.streamCall)
	}
}

func TestHandler_Stream_RecvError_PropagatesErrorKind(t *testing.T) {
	captured, recorder := newCaptureAuditSink()
	callCaptured, callSink := newCaptureCallSink()
	streamObj := fake.NewStream(
		fake.WithEvents([]*llmtypes.Event{
			{Choices: []llmtypes.ChoiceDelta{{Delta: llmtypes.Delta{Content: "partial"}}}},
		}),
		fake.WithRecvErr(&llmtypes.Error{Kind: llmtypes.KindUpstream, Message: "boom mid-stream"}),
		fake.WithSummary(&llmtypes.Summary{}),
	)
	r := &fakeService{
		buildStreamResult: func(req *llmtypes.Request) (*llmrouter.RouteResult, error) {
			return &llmrouter.RouteResult{
				Stream:    streamObj,
				Vendor:    "opencode",
				ModelUsed: req.Model,
				Attempts: []llmtypes.Attempt{
					{Vendor: "opencode", Model: req.Model, StartedAt: time.Now()},
				},
			}, nil
		},
	}
	h := NewHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)), telemetry.NewFanoutSink(nil, recorder, callSink), HandlerConfig{})

	body := `{"model":"deepseek-v4-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	// HTTP 200 was already written before the error — error rides as an
	// SSE frame, then [DONE] terminates.
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (header already flushed before mid-stream err)", w.Code)
	}
	body2 := w.Body.String()
	if !strings.Contains(body2, `"partial"`) {
		t.Errorf("body missing pre-error chunk: %q", body2)
	}
	if !strings.Contains(body2, `"type":"upstream"`) {
		t.Errorf("body missing upstream error envelope: %q", body2)
	}
	if !strings.HasSuffix(body2, "data: [DONE]\n\n") {
		t.Errorf("body must end with [DONE]: %q", body2)
	}

	got := captured.last(t)
	if got.Kind != llmtypes.KindUpstream {
		t.Errorf("rec.Kind = %q, want upstream", got.Kind)
	}
	gotCall := callCaptured.last(t)
	if len(gotCall.Attempts) != 1 || gotCall.Attempts[0].Kind != llmtypes.KindUpstream {
		t.Errorf("Attempts[0].Kind not propagated: %+v", gotCall.Attempts)
	}
}

func TestHandler_Stream_IdleTimeoutSendsError(t *testing.T) {
	captured, recorder := newCaptureAuditSink()
	callCaptured, callSink := newCaptureCallSink()
	streamObj := fake.NewStream(
		fake.WithRecvDelay(50*time.Millisecond),
		fake.WithSummary(&llmtypes.Summary{}),
	)
	r := &fakeService{
		buildStreamResult: func(req *llmtypes.Request) (*llmrouter.RouteResult, error) {
			return &llmrouter.RouteResult{
				Stream:    streamObj,
				Vendor:    "opencode",
				ModelUsed: req.Model,
				Attempts: []llmtypes.Attempt{
					{Vendor: "opencode", Model: req.Model, StartedAt: time.Now()},
				},
			}, nil
		},
	}
	h := NewHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)), telemetry.NewFanoutSink(nil, recorder, callSink), HandlerConfig{
		StreamIdleTimeout: time.Millisecond,
	})

	body := `{"model":"deepseek-v4-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (stream already started)", w.Code)
	}
	body2 := w.Body.String()
	if !strings.Contains(body2, `"type":"timeout"`) {
		t.Errorf("body missing timeout error envelope: %q", body2)
	}
	if !strings.HasSuffix(body2, "data: [DONE]\n\n") {
		t.Errorf("body must end with [DONE]: %q", body2)
	}

	got := captured.last(t)
	if got.Kind != llmtypes.KindTimeout {
		t.Errorf("rec.Kind = %q, want timeout", got.Kind)
	}
	gotCall := callCaptured.last(t)
	if len(gotCall.Attempts) != 1 || gotCall.Attempts[0].Kind != llmtypes.KindTimeout {
		t.Errorf("Attempts[0].Kind not propagated: %+v", gotCall.Attempts)
	}
	if streamObj.Closed() == 0 {
		t.Errorf("Stream.Close() calls = 0, want at least 1")
	}
}

func TestHandler_Stream_RequestTimeoutSendsError(t *testing.T) {
	captured, recorder := newCaptureAuditSink()
	callCaptured, callSink := newCaptureCallSink()
	streamObj := fake.NewStream(
		fake.WithRecvDelay(50*time.Millisecond),
		fake.WithSummary(&llmtypes.Summary{}),
	)
	r := &fakeService{
		buildStreamResult: func(req *llmtypes.Request) (*llmrouter.RouteResult, error) {
			return &llmrouter.RouteResult{
				Stream:    streamObj,
				Vendor:    "opencode",
				ModelUsed: req.Model,
				Attempts: []llmtypes.Attempt{
					{Vendor: "opencode", Model: req.Model, StartedAt: time.Now()},
				},
			}, nil
		},
	}
	h := NewHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)), telemetry.NewFanoutSink(nil, recorder, callSink), HandlerConfig{
		RequestTimeout: time.Millisecond,
	})

	body := `{"model":"deepseek-v4-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (stream already started)", w.Code)
	}
	body2 := w.Body.String()
	if !strings.Contains(body2, `"type":"timeout"`) {
		t.Errorf("body missing timeout error envelope: %q", body2)
	}
	if !strings.HasSuffix(body2, "data: [DONE]\n\n") {
		t.Errorf("body must end with [DONE]: %q", body2)
	}

	got := captured.last(t)
	if got.Kind != llmtypes.KindTimeout {
		t.Errorf("rec.Kind = %q, want timeout", got.Kind)
	}
	gotCall := callCaptured.last(t)
	if len(gotCall.Attempts) != 1 || gotCall.Attempts[0].Kind != llmtypes.KindTimeout {
		t.Errorf("Attempts[0].Kind not propagated: %+v", gotCall.Attempts)
	}
}

func TestHandler_Stream_ContextCanceledRecordsClientClosed(t *testing.T) {
	captured, recorder := newCaptureAuditSink()
	callCaptured, callSink := newCaptureCallSink()
	streamObj := fake.NewStream(
		fake.WithRecvDelay(50*time.Millisecond),
		fake.WithSummary(&llmtypes.Summary{}),
	)
	r := &fakeService{
		buildStreamResult: func(req *llmtypes.Request) (*llmrouter.RouteResult, error) {
			return &llmrouter.RouteResult{
				Stream:    streamObj,
				Vendor:    "opencode",
				ModelUsed: req.Model,
				Attempts: []llmtypes.Attempt{
					{Vendor: "opencode", Model: req.Model, StartedAt: time.Now()},
				},
			}, nil
		},
	}
	h := NewHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)), telemetry.NewFanoutSink(nil, recorder, callSink), HandlerConfig{})

	body := `{"model":"deepseek-v4-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	baseReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	ctx, cancel := context.WithCancel(baseReq.Context())
	req := baseReq.WithContext(ctx)
	cancel()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	got := captured.last(t)
	if got.Kind != llmtypes.KindClientClosed {
		t.Fatalf("Kind = %q, want client_closed", got.Kind)
	}
	if callCaptured.last(t).Kind != llmtypes.KindClientClosed {
		t.Fatalf("call Kind = %q, want client_closed", callCaptured.last(t).Kind)
	}
	if streamObj.Closed() == 0 {
		t.Errorf("Stream.Close() calls = 0, want at least 1")
	}
}

// TestHandler_Stream_ClientDisconnect_MidStream simulates a client that
// hangs up after the first SSE frame. The handler must (a) record the
// terminal state as KindClientClosed in audit and (b) stop draining the
// upstream stream — leaving later events un-consumed.
func TestHandler_Stream_ClientDisconnect_MidStream(t *testing.T) {
	captured, recorder := newCaptureAuditSink()
	_, callSink := newCaptureCallSink()
	streamObj := fake.NewStream(
		fake.WithEvents([]*llmtypes.Event{
			{Choices: []llmtypes.ChoiceDelta{{Delta: llmtypes.Delta{Content: "one"}}}},
			{Choices: []llmtypes.ChoiceDelta{{Delta: llmtypes.Delta{Content: " two"}}}},
			{Choices: []llmtypes.ChoiceDelta{{Delta: llmtypes.Delta{Content: " three"}}}},
		}),
		fake.WithSummary(&llmtypes.Summary{}),
	)
	r := &fakeService{
		buildStreamResult: func(req *llmtypes.Request) (*llmrouter.RouteResult, error) {
			return &llmrouter.RouteResult{
				Stream:    streamObj,
				Vendor:    "opencode",
				ModelUsed: req.Model,
				Attempts: []llmtypes.Attempt{
					{Vendor: "opencode", Model: req.Model, StartedAt: time.Now()},
				},
			}, nil
		},
	}
	h := NewHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)), telemetry.NewFanoutSink(nil, recorder, callSink), HandlerConfig{})

	body := `{"model":"deepseek-v4-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	// Allow one frame through; the next Send call fails.
	w := newDisconnectAfterN(1)
	h.ServeHTTP(w, req)

	got := captured.last(t)
	if got.Kind != llmtypes.KindClientClosed {
		t.Fatalf("Kind = %q, want client_closed", got.Kind)
	}
	// Loop runs two Recvs: events[0] sends OK, events[1] send fails, handler
	// bails. events[2] must remain unread.
	if streamObj.Cursor() != 2 {
		t.Errorf("stream cursor = %d, want 2 (two Recv calls before bail-out)", streamObj.Cursor())
	}
	if streamObj.Closed() == 0 {
		t.Errorf("Stream.Close() not called (defer must run)")
	}
}

// TestHandler_Stream_ClientDisconnect_OnDone covers the EOF success
// path: stream drains cleanly but the [DONE] sentinel write fails.
// Audit must still record client_closed — the wire handshake didn't
// complete even though upstream finished cleanly.
func TestHandler_Stream_ClientDisconnect_OnDone(t *testing.T) {
	captured, recorder := newCaptureAuditSink()
	_, callSink := newCaptureCallSink()
	streamObj := fake.NewStream(
		fake.WithEvents([]*llmtypes.Event{
			{Choices: []llmtypes.ChoiceDelta{{Delta: llmtypes.Delta{Content: "only"}}}},
		}),
		fake.WithSummary(&llmtypes.Summary{}),
	)
	r := &fakeService{
		buildStreamResult: func(req *llmtypes.Request) (*llmrouter.RouteResult, error) {
			return &llmrouter.RouteResult{
				Stream:    streamObj,
				Vendor:    "opencode",
				ModelUsed: req.Model,
				Attempts: []llmtypes.Attempt{
					{Vendor: "opencode", Model: req.Model, StartedAt: time.Now()},
				},
			}, nil
		},
	}
	h := NewHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)), telemetry.NewFanoutSink(nil, recorder, callSink), HandlerConfig{})

	body := `{"model":"deepseek-v4-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	// Allow the only event through (1 write); next Write (the [DONE]) fails.
	w := newDisconnectAfterN(1)
	h.ServeHTTP(w, req)

	got := captured.last(t)
	if got.Kind != llmtypes.KindClientClosed {
		t.Errorf("Kind = %q, want client_closed (SendDone failed)", got.Kind)
	}
}

// TestHandler_Stream_ClientDisconnect_OnFirstEvent covers the path where
// the very first SSE write fails — handler should bail after consuming
// just the first event.
func TestHandler_Stream_ClientDisconnect_OnFirstEvent(t *testing.T) {
	captured, recorder := newCaptureAuditSink()
	_, callSink := newCaptureCallSink()
	streamObj := fake.NewStream(
		fake.WithEvents([]*llmtypes.Event{
			{Choices: []llmtypes.ChoiceDelta{{Delta: llmtypes.Delta{Content: "first"}}}},
			{Choices: []llmtypes.ChoiceDelta{{Delta: llmtypes.Delta{Content: "later"}}}},
		}),
		fake.WithSummary(&llmtypes.Summary{}),
	)
	r := &fakeService{
		buildStreamResult: func(req *llmtypes.Request) (*llmrouter.RouteResult, error) {
			return &llmrouter.RouteResult{
				Stream:    streamObj,
				Vendor:    "opencode",
				ModelUsed: req.Model,
				Attempts: []llmtypes.Attempt{
					{Vendor: "opencode", Model: req.Model, StartedAt: time.Now()},
				},
			}, nil
		},
	}
	h := NewHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)), telemetry.NewFanoutSink(nil, recorder, callSink), HandlerConfig{})

	body := `{"model":"deepseek-v4-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := newDisconnectAfterN(0)
	h.ServeHTTP(w, req)

	got := captured.last(t)
	if got.Kind != llmtypes.KindClientClosed {
		t.Fatalf("Kind = %q, want client_closed", got.Kind)
	}
	if streamObj.Cursor() != 1 {
		t.Errorf("stream cursor = %d, want 1 (one Recv before first Send fails)", streamObj.Cursor())
	}
}

// TestHandler_NonStream_ClientDisconnect verifies the JSON response
// path also tags audit when the client write fails. StatusCode stays 200
// (already on the wire), but ErrorKind reveals the terminal state.
func TestHandler_NonStream_ClientDisconnect(t *testing.T) {
	captured, recorder := newCaptureAuditSink()
	_, callSink := newCaptureCallSink()
	r := &fakeService{
		buildResult: func(req *llmtypes.Request) *llmrouter.RouteResult {
			return &llmrouter.RouteResult{
				Response: &llmtypes.Response{
					Model:   req.Model,
					Choices: []llmtypes.Choice{{Index: 0, Message: llmtypes.Message{Role: "assistant", Content: "ok"}}},
				},
				Vendor:    "opencode",
				ModelUsed: req.Model,
				Attempts: []llmtypes.Attempt{
					{Vendor: "opencode", Model: req.Model, StatusCode: 200, StartedAt: time.Now()},
				},
			}
		},
	}
	h := NewHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)), telemetry.NewFanoutSink(nil, recorder, callSink), HandlerConfig{})

	body := `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := newDisconnectAfterN(0)
	h.ServeHTTP(w, req)

	got := captured.last(t)
	if got.Kind != llmtypes.KindClientClosed {
		t.Errorf("Kind = %q, want client_closed", got.Kind)
	}
	if got.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200 (already flushed before write failure)", got.StatusCode)
	}
}

// disconnectAfterNWriter accepts the first n Write calls and fails
// every subsequent one with a synthetic broken-pipe error. Implements
// http.ResponseWriter and http.Flusher so handler streaming code paths
// detect it as flushable.
type disconnectAfterNWriter struct {
	rec *httptest.ResponseRecorder
	n   int
	cnt int
}

func newDisconnectAfterN(n int) *disconnectAfterNWriter {
	return &disconnectAfterNWriter{rec: httptest.NewRecorder(), n: n}
}

func (d *disconnectAfterNWriter) Header() http.Header { return d.rec.Header() }

func (d *disconnectAfterNWriter) Write(b []byte) (int, error) {
	if d.cnt >= d.n {
		return 0, errors.New("simulated broken pipe")
	}
	d.cnt++
	return d.rec.Write(b)
}

func (d *disconnectAfterNWriter) WriteHeader(statusCode int) { d.rec.WriteHeader(statusCode) }

func (d *disconnectAfterNWriter) Flush() {}

func TestHandler_Stream_PreStreamServiceError(t *testing.T) {
	captured, recorder := newCaptureAuditSink()
	callCaptured, callSink := newCaptureCallSink()
	r := &fakeService{
		buildStreamResult: func(req *llmtypes.Request) (*llmrouter.RouteResult, error) {
			return &llmrouter.RouteResult{
				Vendor:    "opencode",
				ModelUsed: req.Model,
				Attempts: []llmtypes.Attempt{
					{Vendor: "opencode", Model: req.Model, Kind: llmtypes.KindAuth, StatusCode: 401, StartedAt: time.Now()},
				},
			}, &llmtypes.Error{Kind: llmtypes.KindAuth, Message: "no key"}
		},
	}
	h := NewHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)), telemetry.NewFanoutSink(nil, recorder, callSink), HandlerConfig{})

	body := `{"model":"deepseek-v4-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (pre-stream error → JSON envelope)", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (pre-stream)", got)
	}
	if !strings.Contains(w.Body.String(), `"type":"auth"`) {
		t.Errorf("body missing auth envelope: %q", w.Body.String())
	}

	got := captured.last(t)
	if got.Kind != llmtypes.KindAuth {
		t.Errorf("rec.Kind = %q, want auth", got.Kind)
	}
	if got.StatusCode != http.StatusUnauthorized {
		t.Errorf("rec.StatusCode = %d, want 401", got.StatusCode)
	}
	gotCall := callCaptured.last(t)
	if len(gotCall.Attempts) != 1 || gotCall.Attempts[0].Kind != llmtypes.KindAuth {
		t.Errorf("Attempts[0] = %+v, want auth attempt", gotCall.Attempts)
	}
}

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
	cw := &countingWriter{ResponseWriter: inner, status: http.StatusOK, wroteHeader: true}

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
func TestHandler_recoverPanic_OverridesPreStampedStatus(t *testing.T) {
	_, recorder := newCaptureAuditSink()
	h := NewHandler(&fakeService{vendor: "opencode"}, slog.New(slog.NewTextHandler(io.Discard, nil)), recorder, HandlerConfig{})

	rec := &telemetry.AuditEvent{
		EventCommon: telemetry.EventCommon{
			RequestID:  "test-req-id",
			StatusCode: http.StatusOK,
		},
	}
	inner := httptest.NewRecorder()
	cw := &countingWriter{ResponseWriter: inner, status: http.StatusOK, wroteHeader: true}

	h.recoverPanic(context.Background(), cw, rec, "mid-stream boom")

	if rec.Kind != llmtypes.KindPanic {
		t.Errorf("Kind = %q, want %q", rec.Kind, llmtypes.KindPanic)
	}
	if rec.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want 500 — must override pre-stamped 200 so a panic is not recorded as success", rec.StatusCode)
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
		buildStreamResult: func(req *llmtypes.Request) (*llmrouter.RouteResult, error) {
			return &llmrouter.RouteResult{
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

	body := `{"model":"deepseek-v4-flash","stream":true,"messages":[{"role":"user","content":"Reply with exactly OK."}],"max_tokens":8}`
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

// fakeService implements ChatService for handler tests. buildResult /
// buildStreamResult let each test case shape the RouteResult —
// including pre-populated Attempts — so we exercise the audit-copy
// path without spinning up a real Service.
type fakeService struct {
	vendor            string
	buildResult       func(req *llmtypes.Request) *llmrouter.RouteResult
	buildStreamResult func(req *llmtypes.Request) (*llmrouter.RouteResult, error)
}

func (f *fakeService) Complete(_ context.Context, req *llmtypes.Request) (*llmrouter.RouteResult, error) {
	return f.buildResult(req), nil
}

func (f *fakeService) CompleteStream(_ context.Context, req *llmtypes.Request) (*llmrouter.RouteResult, error) {
	if f.buildStreamResult != nil {
		return f.buildStreamResult(req)
	}
	return &llmrouter.RouteResult{}, &llmtypes.Error{Kind: llmtypes.KindUpstream, Message: "stream not implemented in this fake"}
}

func okFakeService() *fakeService {
	return &fakeService{
		vendor: "opencode",
		buildResult: func(req *llmtypes.Request) *llmrouter.RouteResult {
			return &llmrouter.RouteResult{
				Response: &llmtypes.Response{
					Model:   req.Model,
					Choices: []llmtypes.Choice{{Index: 0, Message: llmtypes.Message{Role: "assistant", Content: "ok"}}},
				},
				Vendor:    "opencode",
				ModelUsed: req.Model,
				Attempts: []llmtypes.Attempt{
					{Vendor: "opencode", Model: req.Model, StatusCode: http.StatusOK, StartedAt: time.Now()},
				},
			}
		},
	}
}

type panicEventSink struct{}

func (panicEventSink) Emit(context.Context, telemetry.Event) { panic("telemetry sink failed") }
func (panicEventSink) Close() error                          { return nil }

type panicLifecycleObserver struct{}

func (panicLifecycleObserver) RequestStarted(context.Context) { panic("request started failed") }
func (panicLifecycleObserver) RequestFinished(context.Context) {
	panic("request finished failed")
}
func (panicLifecycleObserver) StreamStarted(context.Context, telemetry.EventCommon) {
	panic("stream started failed")
}
func (panicLifecycleObserver) StreamFinished(context.Context, *telemetry.AuditEvent, *telemetry.CallEvent) {
	panic("stream finished failed")
}

type captureAuditSink struct {
	mu      sync.Mutex
	records []*telemetry.AuditEvent
}

func newCaptureAuditSink() (*captureAuditSink, telemetry.EventSink) {
	c := &captureAuditSink{}
	return c, c
}

func (c *captureAuditSink) captureAudit(_ context.Context, r *telemetry.AuditEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r)
}

func (c *captureAuditSink) Close() error { return nil }

func (c *captureAuditSink) Emit(ctx context.Context, event telemetry.Event) {
	rec, ok := event.(*telemetry.AuditEvent)
	if !ok {
		return
	}
	c.captureAudit(ctx, rec)
}

func (c *captureAuditSink) last(t *testing.T) *telemetry.AuditEvent {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.records) == 0 {
		t.Fatalf("no records captured")
	}
	return c.records[len(c.records)-1]
}

type captureCallSink struct {
	mu    sync.Mutex
	calls []*telemetry.CallEvent
}

func newCaptureCallSink() (*captureCallSink, telemetry.EventSink) {
	c := &captureCallSink{}
	return c, c
}

func (c *captureCallSink) captureCall(_ context.Context, r *telemetry.CallEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, r)
}

func (c *captureCallSink) Close() error { return nil }

func (c *captureCallSink) Emit(ctx context.Context, event telemetry.Event) {
	rec, ok := event.(*telemetry.CallEvent)
	if !ok {
		return
	}
	c.captureCall(ctx, rec)
}

func (c *captureCallSink) last(t *testing.T) *telemetry.CallEvent {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.calls) == 0 {
		t.Fatalf("no call records captured")
	}
	return c.calls[len(c.calls)-1]
}

func (c *captureCallSink) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func newLogContractSink() (*bytes.Buffer, *bytes.Buffer, telemetry.EventSink) {
	auditBuf := &bytes.Buffer{}
	callBuf := &bytes.Buffer{}
	auditLog := slog.New(slog.NewJSONHandler(auditBuf, nil)).With(slog.String("log", "audit"))
	callLog := slog.New(slog.NewJSONHandler(callBuf, nil)).With(slog.String("log", "call"))
	return auditBuf, callBuf, telemetry.NewSlogSink(auditLog, callLog)
}

func decodeSingleLogLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 || lines[0] == "" {
		t.Fatalf("log lines = %d, want 1; logs=%q", len(lines), buf.String())
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &out); err != nil {
		t.Fatalf("decode log line: %v; line=%s", err, lines[0])
	}
	return out
}

func requestWithTelemetryContext(req *http.Request, requestID string, consumer *ConsumerInfo) *http.Request {
	ctx := context.WithValue(req.Context(), requestIDCtxKey{}, requestID)
	ctx = context.WithValue(ctx, consumerCtxKey{}, consumer)
	return req.WithContext(ctx)
}

func assertSuccessAuditLog(t *testing.T, got map[string]any, requestID, operation string) {
	t.Helper()
	wantLogField(t, got, "log", "audit")
	wantLogField(t, got, "event_type", "audit")
	wantLogField(t, got, "request_id", requestID)
	wantLogField(t, got, "operation", operation)
	wantLogNumber(t, got, "status", http.StatusOK)
	wantLogField(t, got, "consumer_name", "example")
	wantLogField(t, got, "consumer_key_id", "467d813a")
	wantLogField(t, got, "auth_result", "success")
	wantLogField(t, got, "policy_result", "allowed")
	wantLogField(t, got, "resource_type", "llm_model")
	wantLogField(t, got, "resource_id", "deepseek-v4-flash")
}

func assertSuccessCallLog(t *testing.T, got map[string]any, requestID, operation string) {
	t.Helper()
	wantLogField(t, got, "log", "call")
	wantLogField(t, got, "event_type", "call")
	wantLogField(t, got, "request_id", requestID)
	wantLogField(t, got, "operation", operation)
	wantLogNumber(t, got, "status", http.StatusOK)
	wantLogField(t, got, "consumer_name", "example")
	wantLogField(t, got, "consumer_key_id", "467d813a")
	wantLogField(t, got, "model_requested", "deepseek-v4-flash")
	wantLogField(t, got, "vendor", "opencode")
	wantLogField(t, got, "final_attempt_vendor", "opencode")
	wantLogField(t, got, "final_attempt_model", "deepseek-v4-flash")
	wantLogNumber(t, got, "attempts_count", 1)
}

func wantLogField(t *testing.T, got map[string]any, key string, want string) {
	t.Helper()
	if got[key] != want {
		t.Fatalf("%s = %v, want %q; log=%+v", key, got[key], want, got)
	}
}

func wantLogNumber(t *testing.T, got map[string]any, key string, want int) {
	t.Helper()
	val, ok := got[key].(float64)
	if !ok {
		t.Fatalf("%s = %T(%v), want number %d; log=%+v", key, got[key], got[key], want, got)
	}
	if int(val) != want {
		t.Fatalf("%s = %v, want %d; log=%+v", key, val, want, got)
	}
}

func assertLogDoesNotContainSensitiveMaterial(t *testing.T, bufs ...*bytes.Buffer) {
	t.Helper()
	var joined strings.Builder
	for _, buf := range bufs {
		joined.WriteString(buf.String())
	}
	logged := joined.String()
	for _, forbidden := range []string{
		"Authorization",
		"Bearer ",
		"example-key-001",
		"Reply with exactly OK.",
		"say ok",
	} {
		if strings.Contains(logged, forbidden) {
			t.Fatalf("log leaked %q: %s", forbidden, logged)
		}
	}
}

type captureLifecycle struct {
	requestStarted  int
	requestFinished int
	streamStarted   int
	streamFinished  int
	streamCommon    telemetry.EventCommon
	streamAudit     *telemetry.AuditEvent
	streamCall      *telemetry.CallEvent
}

func (c *captureLifecycle) RequestStarted(context.Context) {
	c.requestStarted++
}

func (c *captureLifecycle) RequestFinished(context.Context) {
	c.requestFinished++
}

func (c *captureLifecycle) StreamStarted(_ context.Context, common telemetry.EventCommon) {
	c.streamStarted++
	c.streamCommon = common
}

func (c *captureLifecycle) StreamFinished(_ context.Context, audit *telemetry.AuditEvent, call *telemetry.CallEvent) {
	c.streamFinished++
	c.streamAudit = audit
	c.streamCall = call
}
