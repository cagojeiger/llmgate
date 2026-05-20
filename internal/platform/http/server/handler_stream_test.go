package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/routing"
	"llmgate/internal/providers/fake"
)

func TestHandler_Stream_NormalEOF(t *testing.T) {
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
		buildStreamResult: func(req *llmtypes.Request) (*routing.RouteResult, error) {
			return streamRouteResult(req, streamObj), nil
		},
	}
	h := newHandlerHarness(r, HandlerConfig{
		LifecycleObserver: lifecycle,
	})

	w := h.serve(streamChatBody)

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

	got := h.audit.last(t)
	if got.Operation != "chat.completions.stream" {
		t.Errorf("Operation = %q, want chat.completions.stream", got.Operation)
	}
	if got.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", got.StatusCode)
	}
	gotCall := h.calls.last(t)
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
	streamObj := fake.NewStream(
		fake.WithEvents([]*llmtypes.Event{
			{Choices: []llmtypes.ChoiceDelta{{Delta: llmtypes.Delta{Content: "partial"}}}},
		}),
		fake.WithRecvErr(&llmtypes.Error{Kind: llmtypes.KindUpstream, Message: "boom mid-stream"}),
		fake.WithSummary(&llmtypes.Summary{}),
	)
	r := &fakeService{
		buildStreamResult: func(req *llmtypes.Request) (*routing.RouteResult, error) {
			return streamRouteResult(req, streamObj), nil
		},
	}
	h := newHandlerHarness(r, HandlerConfig{})

	w := h.serve(streamChatBody)

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

	got := h.audit.last(t)
	if got.Kind != llmtypes.KindUpstream {
		t.Errorf("rec.Kind = %q, want upstream", got.Kind)
	}
	gotCall := h.calls.last(t)
	if len(gotCall.Attempts) != 1 || gotCall.Attempts[0].Kind != llmtypes.KindUpstream {
		t.Errorf("Attempts[0].Kind not propagated: %+v", gotCall.Attempts)
	}
}

func TestHandler_Stream_IdleTimeoutSendsError(t *testing.T) {
	streamObj := fake.NewStream(
		fake.WithRecvDelay(50*time.Millisecond),
		fake.WithSummary(&llmtypes.Summary{}),
	)
	r := &fakeService{
		buildStreamResult: func(req *llmtypes.Request) (*routing.RouteResult, error) {
			return streamRouteResult(req, streamObj), nil
		},
	}
	h := newHandlerHarness(r, HandlerConfig{
		StreamIdleTimeout: time.Millisecond,
	})

	w := h.serve(streamChatBody)

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

	got := h.audit.last(t)
	if got.Kind != llmtypes.KindTimeout {
		t.Errorf("rec.Kind = %q, want timeout", got.Kind)
	}
	gotCall := h.calls.last(t)
	if len(gotCall.Attempts) != 1 || gotCall.Attempts[0].Kind != llmtypes.KindTimeout {
		t.Errorf("Attempts[0].Kind not propagated: %+v", gotCall.Attempts)
	}
	if streamObj.Closed() == 0 {
		t.Errorf("Stream.Close() calls = 0, want at least 1")
	}
}

func TestHandler_Stream_RequestTimeoutSendsError(t *testing.T) {
	streamObj := fake.NewStream(
		fake.WithRecvDelay(50*time.Millisecond),
		fake.WithSummary(&llmtypes.Summary{}),
	)
	r := &fakeService{
		buildStreamResult: func(req *llmtypes.Request) (*routing.RouteResult, error) {
			return streamRouteResult(req, streamObj), nil
		},
	}
	h := newHandlerHarness(r, HandlerConfig{
		RequestTimeout: time.Millisecond,
	})

	w := h.serve(streamChatBody)

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

	got := h.audit.last(t)
	if got.Kind != llmtypes.KindTimeout {
		t.Errorf("rec.Kind = %q, want timeout", got.Kind)
	}
	gotCall := h.calls.last(t)
	if len(gotCall.Attempts) != 1 || gotCall.Attempts[0].Kind != llmtypes.KindTimeout {
		t.Errorf("Attempts[0].Kind not propagated: %+v", gotCall.Attempts)
	}
}

func TestHandler_Stream_ContextCanceledRecordsClientClosed(t *testing.T) {
	streamObj := fake.NewStream(
		fake.WithRecvDelay(50*time.Millisecond),
		fake.WithSummary(&llmtypes.Summary{}),
	)
	r := &fakeService{
		buildStreamResult: func(req *llmtypes.Request) (*routing.RouteResult, error) {
			return streamRouteResult(req, streamObj), nil
		},
	}
	h := newHandlerHarness(r, HandlerConfig{})

	baseReq := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(streamChatBody))
	ctx, cancel := context.WithCancel(baseReq.Context())
	req := baseReq.WithContext(ctx)
	cancel()

	h.serveRequest(req)

	got := h.audit.last(t)
	if got.Kind != llmtypes.KindClientClosed {
		t.Fatalf("Kind = %q, want client_closed", got.Kind)
	}
	if h.calls.last(t).Kind != llmtypes.KindClientClosed {
		t.Fatalf("call Kind = %q, want client_closed", h.calls.last(t).Kind)
	}
	if streamObj.Closed() == 0 {
		t.Errorf("Stream.Close() calls = 0, want at least 1")
	}
}

func TestHandler_Stream_PreStreamServiceError(t *testing.T) {
	r := &fakeService{
		buildStreamResult: func(req *llmtypes.Request) (*routing.RouteResult, error) {
			return &routing.RouteResult{
				Vendor:    "opencode",
				ModelUsed: req.Model,
				Attempts: []llmtypes.Attempt{
					{Vendor: "opencode", Model: req.Model, Kind: llmtypes.KindAuth, StatusCode: 401, StartedAt: time.Now()},
				},
			}, &llmtypes.Error{Kind: llmtypes.KindAuth, Message: "no key"}
		},
	}
	h := newHandlerHarness(r, HandlerConfig{})

	w := h.serve(streamChatBody)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (pre-stream error → JSON envelope)", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (pre-stream)", got)
	}
	if !strings.Contains(w.Body.String(), `"type":"auth"`) {
		t.Errorf("body missing auth envelope: %q", w.Body.String())
	}

	got := h.audit.last(t)
	if got.Kind != llmtypes.KindAuth {
		t.Errorf("rec.Kind = %q, want auth", got.Kind)
	}
	if got.StatusCode != http.StatusUnauthorized {
		t.Errorf("rec.StatusCode = %d, want 401", got.StatusCode)
	}
	gotCall := h.calls.last(t)
	if len(gotCall.Attempts) != 1 || gotCall.Attempts[0].Kind != llmtypes.KindAuth {
		t.Errorf("Attempts[0] = %+v, want auth attempt", gotCall.Attempts)
	}
}
