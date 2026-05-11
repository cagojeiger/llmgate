package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"llmgate/internal/audit"
	"llmgate/internal/llmrouter"
	"llmgate/internal/llmtypes"
	"llmgate/internal/providers/fake"
	"llmgate/internal/streaming"
)

func TestHandler_SingleAttempt_RecordPopulated(t *testing.T) {
	rec, recorder := newCaptureRecorder()
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
	h := NewHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)), recorder, HandlerConfig{})

	body := `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	got := rec.last(t)
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
	rec, recorder := newCaptureRecorder()
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
	h := NewHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)), recorder, HandlerConfig{})

	body := `{"model":"coder","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	got := rec.last(t)
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
		{"rate_limit", llmtypes.KindRateLimit, http.StatusTooManyRequests},
		{"bad_request", llmtypes.KindBadRequest, http.StatusBadRequest},
		{"context_length", llmtypes.KindContextLength, http.StatusBadRequest},
		{"upstream", llmtypes.KindUpstream, http.StatusBadGateway},
		{"timeout", llmtypes.KindTimeout, http.StatusBadGateway},
		{"unknown", llmtypes.KindUnknown, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &audit.Record{}
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

func TestAdoptError_NonProviderError_Falls500Unknown(t *testing.T) {
	rec := &audit.Record{}
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
	rec := &audit.Record{
		Attempts: []llmtypes.Attempt{
			{Vendor: "v", Model: "m", StartedAt: started},
		},
	}
	sum := &llmtypes.Summary{
		Usage:      &llmtypes.Usage{PromptTokens: 5, CompletionTokens: 7, TotalTokens: 12},
		VendorCost: `"0.001"`,
	}

	adoptStreamSummary(rec, sum, now)

	if rec.Usage == nil || rec.Usage.TotalTokens != 12 {
		t.Errorf("rec.Usage = %+v, want total=12", rec.Usage)
	}
	if rec.VendorCost != `"0.001"` {
		t.Errorf("rec.VendorCost = %q, want \"0.001\"", rec.VendorCost)
	}
	last := rec.Attempts[0]
	if last.DurationMS != 250 {
		t.Errorf("last.DurationMS = %d, want 250", last.DurationMS)
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
	rec := &audit.Record{
		Kind: llmtypes.KindUpstream,
		Attempts: []llmtypes.Attempt{
			{Vendor: "v", Model: "m", StartedAt: started},
		},
	}

	adoptStreamSummary(rec, nil, now)

	if rec.Attempts[0].Kind != llmtypes.KindUpstream {
		t.Errorf("attempt ErrorKind = %q, want upstream", rec.Attempts[0].Kind)
	}
	if rec.Attempts[0].DurationMS != 100 {
		t.Errorf("DurationMS = %d, want 100", rec.Attempts[0].DurationMS)
	}
}

func TestHandler_Stream_NormalEOF(t *testing.T) {
	captured, recorder := newCaptureRecorder()
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
	h := NewHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)), recorder, HandlerConfig{})

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
	if got.Usage == nil || got.Usage.TotalTokens != 5 {
		t.Errorf("Usage = %+v, want total=5 from Summary", got.Usage)
	}
	if len(got.Attempts) != 1 {
		t.Fatalf("Attempts = %d, want 1", len(got.Attempts))
	}
	if got.Attempts[0].Usage == nil || got.Attempts[0].Usage.TotalTokens != 5 {
		t.Errorf("Attempts[0].Usage not finalized from Summary: %+v", got.Attempts[0].Usage)
	}
	if got.ResponseBytes <= 0 {
		t.Errorf("ResponseBytes = %d, want > 0", got.ResponseBytes)
	}
}

func TestHandler_Stream_RecvError_PropagatesErrorKind(t *testing.T) {
	captured, recorder := newCaptureRecorder()
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
	h := NewHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)), recorder, HandlerConfig{})

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
	if len(got.Attempts) != 1 || got.Attempts[0].Kind != llmtypes.KindUpstream {
		t.Errorf("Attempts[0].Kind not propagated: %+v", got.Attempts)
	}
}

func TestHandler_Stream_IdleTimeoutSendsError(t *testing.T) {
	captured, recorder := newCaptureRecorder()
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
	h := NewHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)), recorder, HandlerConfig{
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
	if len(got.Attempts) != 1 || got.Attempts[0].Kind != llmtypes.KindTimeout {
		t.Errorf("Attempts[0].Kind not propagated: %+v", got.Attempts)
	}
	if streamObj.Closed() == 0 {
		t.Errorf("Stream.Close() calls = 0, want at least 1")
	}
}

func TestHandler_Stream_RequestTimeoutSendsError(t *testing.T) {
	captured, recorder := newCaptureRecorder()
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
	h := NewHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)), recorder, HandlerConfig{
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
	if len(got.Attempts) != 1 || got.Attempts[0].Kind != llmtypes.KindTimeout {
		t.Errorf("Attempts[0].Kind not propagated: %+v", got.Attempts)
	}
}

func TestHandler_Stream_ContextCanceledRecordsClientClosed(t *testing.T) {
	captured, recorder := newCaptureRecorder()
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
	h := NewHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)), recorder, HandlerConfig{})

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
	if streamObj.Closed() == 0 {
		t.Errorf("Stream.Close() calls = 0, want at least 1")
	}
}

// TestHandler_Stream_ClientDisconnect_MidStream simulates a client that
// hangs up after the first SSE frame. The handler must (a) record the
// terminal state as KindClientClosed in audit and (b) stop draining the
// upstream stream — leaving later events un-consumed.
func TestHandler_Stream_ClientDisconnect_MidStream(t *testing.T) {
	captured, recorder := newCaptureRecorder()
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
	h := NewHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)), recorder, HandlerConfig{})

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
	captured, recorder := newCaptureRecorder()
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
	h := NewHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)), recorder, HandlerConfig{})

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
	captured, recorder := newCaptureRecorder()
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
	h := NewHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)), recorder, HandlerConfig{})

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
	captured, recorder := newCaptureRecorder()
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
	h := NewHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)), recorder, HandlerConfig{})

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
	captured, recorder := newCaptureRecorder()
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
	h := NewHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)), recorder, HandlerConfig{})

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
	if len(got.Attempts) != 1 || got.Attempts[0].Kind != llmtypes.KindAuth {
		t.Errorf("Attempts[0] = %+v, want auth attempt", got.Attempts)
	}
}

// TestHandler_PanicInComplete_StampsAuditAndReturns500 locks in the
// audit-always invariant (ADR 003) for the panic path: when the
// handler's downstream service panics, the deferred audit record
// must surface as KindPanic / status 500 rather than at its
// last-assigned (often empty / 0) values, so panic spikes are
// observable in the audit stream and aren't indistinguishable from
// other failures during forensics.
//
// Also pins the wire surface — generic 500 envelope, no panic value
// or stack leakage to the caller. Panic internals only land in the
// slog stream where operators expect them.
func TestHandler_PanicInComplete_StampsAuditAndReturns500(t *testing.T) {
	rec, recorder := newCaptureRecorder()
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
		t.Errorf("Kind = %q, want %q (audit-always invariant)", got.Kind, llmtypes.KindPanic)
	}
	if got.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want 500", got.StatusCode)
	}
	if strings.Contains(w.Body.String(), "boom") {
		t.Errorf("wire body leaked panic value: %s", w.Body.String())
	}
}

// TestHandler_PanicInStream_StampsAuditAndReturns500 covers the same
// invariant on the streaming entry path. The panic happens before
// any SSE bytes are flushed, so the wire status is a clean 500;
// audit still gets KindPanic.
func TestHandler_PanicInStream_StampsAuditAndReturns500(t *testing.T) {
	rec, recorder := newCaptureRecorder()
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

type captureRecorder struct {
	mu      sync.Mutex
	records []*audit.Record
}

func newCaptureRecorder() (*captureRecorder, audit.Recorder) {
	c := &captureRecorder{}
	return c, c
}

func (c *captureRecorder) Record(_ context.Context, r *audit.Record) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r)
}

func (c *captureRecorder) Close() error { return nil }

func (c *captureRecorder) last(t *testing.T) *audit.Record {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.records) == 0 {
		t.Fatalf("no records captured")
	}
	return c.records[len(c.records)-1]
}

// stubbornStream simulates a misbehaving adapter whose Close does not
// unblock a pending Recv. Used to verify recvWithIdleTimeout's bounded
// wait safety net.
type stubbornStream struct {
	closeCalled int32
	block       chan struct{}
}

func newStubbornStream() *stubbornStream {
	return &stubbornStream{block: make(chan struct{})}
}

func (s *stubbornStream) Recv() (*llmtypes.Event, error) {
	<-s.block
	return nil, io.EOF
}

func (s *stubbornStream) Close() error {
	s.closeCalled++
	return nil
}

func (s *stubbornStream) Summary() *llmtypes.Summary { return &llmtypes.Summary{} }

func (s *stubbornStream) release() { close(s.block) }

func TestRecvWithIdleTimeout_BoundedDrainOnContextCancel(t *testing.T) {
	prev := streaming.CloseGrace
	streaming.CloseGrace = 50 * time.Millisecond
	defer func() { streaming.CloseGrace = prev }()

	s := newStubbornStream()
	defer s.release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	_, err := recvWithIdleTimeout(ctx, s, 0)
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Fatalf("recvWithIdleTimeout returned in %v, want < 500ms (grace=50ms)", elapsed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if s.closeCalled == 0 {
		t.Errorf("Stream.Close() not invoked")
	}
}

func TestRecvWithIdleTimeout_BoundedDrainOnIdleTimeout(t *testing.T) {
	prev := streaming.CloseGrace
	streaming.CloseGrace = 50 * time.Millisecond
	defer func() { streaming.CloseGrace = prev }()

	s := newStubbornStream()
	defer s.release()

	start := time.Now()
	_, err := recvWithIdleTimeout(context.Background(), s, 20*time.Millisecond)
	elapsed := time.Since(start)

	// Idle timer fires (~20ms) → Close → 50ms grace → return. Total ~70ms.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("recvWithIdleTimeout returned in %v, want < 500ms", elapsed)
	}
	var perr *llmtypes.Error
	if !errors.As(err, &perr) || perr.Kind != llmtypes.KindTimeout {
		t.Errorf("err = %v, want KindTimeout llmtypes.Error", err)
	}
	if s.closeCalled == 0 {
		t.Errorf("Stream.Close() not invoked on idle timeout")
	}
}
