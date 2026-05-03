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
	"llmgate/internal/provider"
	"llmgate/internal/router"
)

func TestHandler_SingleAttempt_RecordPopulated(t *testing.T) {
	rec, recorder := newCaptureRecorder()
	r := &fakeRouter{
		vendor: "opencode",
		buildResult: func(req *provider.Request) *router.RouteResult {
			return &router.RouteResult{
				Response: &provider.Response{
					Model:   req.Model,
					Choices: []provider.Choice{{Index: 0, Message: provider.Message{Role: "assistant", Content: "ok"}}},
				},
				Vendor:    "opencode",
				ModelUsed: req.Model,
				Attempts: []provider.Attempt{
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
	r := &fakeRouter{
		vendor: "opencode",
		buildResult: func(req *provider.Request) *router.RouteResult {
			return &router.RouteResult{
				Response: &provider.Response{
					Model:   "deepseek-v4-flash",
					Choices: []provider.Choice{{Index: 0, Message: provider.Message{Role: "assistant", Content: "ok"}}},
				},
				Vendor:    "opencode",
				ModelUsed: "deepseek-v4-flash",
				Attempts: []provider.Attempt{
					{Vendor: "opencode", Model: "deepseek-v4-pro", ErrorKind: provider.KindRateLimit, StatusCode: 429, StartedAt: time.Now()},
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
	if got.Attempts[0].ErrorKind != provider.KindRateLimit {
		t.Errorf("attempts[0].ErrorKind = %q, want rate_limit", got.Attempts[0].ErrorKind)
	}
}

func TestAdoptError_ProviderErrorMapsKindAndStatus(t *testing.T) {
	cases := []struct {
		name       string
		kind       provider.Kind
		wantStatus int
	}{
		{"auth", provider.KindAuth, http.StatusUnauthorized},
		{"rate_limit", provider.KindRateLimit, http.StatusTooManyRequests},
		{"bad_request", provider.KindBadRequest, http.StatusBadRequest},
		{"context_length", provider.KindContextLength, http.StatusBadRequest},
		{"upstream", provider.KindUpstream, http.StatusBadGateway},
		{"timeout", provider.KindTimeout, http.StatusBadGateway},
		{"unknown", provider.KindUnknown, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &audit.Record{}
			adoptError(rec, &provider.Error{Kind: tc.kind, Message: "x"})
			if rec.ErrorKind != tc.kind {
				t.Errorf("ErrorKind = %q, want %q", rec.ErrorKind, tc.kind)
			}
			if rec.StatusCode != tc.wantStatus {
				t.Errorf("StatusCode = %d, want %d", rec.StatusCode, tc.wantStatus)
			}
		})
	}
}

func TestAdoptError_NonProviderError_Falls500(t *testing.T) {
	rec := &audit.Record{}
	adoptError(rec, io.ErrUnexpectedEOF)
	if rec.ErrorKind != "" {
		t.Errorf("ErrorKind = %q, want empty (non-provider err shouldn't set kind)", rec.ErrorKind)
	}
	if rec.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want 500", rec.StatusCode)
	}
}

func TestAdoptStreamSummary_FinalizesAttemptAndRecord(t *testing.T) {
	started := time.Unix(1700000000, 0)
	now := started.Add(250 * time.Millisecond)
	rec := &audit.Record{
		Attempts: []provider.Attempt{
			{Vendor: "v", Model: "m", StartedAt: started},
		},
	}
	sum := &provider.Summary{
		Usage:      &provider.Usage{PromptTokens: 5, CompletionTokens: 7, TotalTokens: 12},
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
	// Recv loop set rec.ErrorKind; the deferred summary sync must mirror
	// it onto the in-flight Attempt so audit logs stay symmetric with the
	// non-stream path.
	started := time.Unix(1700000000, 0)
	now := started.Add(100 * time.Millisecond)
	rec := &audit.Record{
		ErrorKind: provider.KindUpstream,
		Attempts: []provider.Attempt{
			{Vendor: "v", Model: "m", StartedAt: started},
		},
	}

	adoptStreamSummary(rec, nil, now)

	if rec.Attempts[0].ErrorKind != provider.KindUpstream {
		t.Errorf("attempt ErrorKind = %q, want upstream", rec.Attempts[0].ErrorKind)
	}
	if rec.Attempts[0].DurationMS != 100 {
		t.Errorf("DurationMS = %d, want 100", rec.Attempts[0].DurationMS)
	}
}

func TestHandler_Stream_NormalEOF(t *testing.T) {
	captured, recorder := newCaptureRecorder()
	streamObj := &fakeStream{
		events: []*provider.Event{
			{Choices: []provider.ChoiceDelta{{Delta: provider.Delta{Content: " world"}}}},
		},
		summary: &provider.Summary{
			Usage: &provider.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5},
		},
	}
	r := &fakeRouter{
		buildStreamResult: func(req *provider.Request) (*router.RouteResult, error) {
			return &router.RouteResult{
				Stream:     streamObj,
				FirstEvent: &provider.Event{Choices: []provider.ChoiceDelta{{Delta: provider.Delta{Content: "hello"}}}},
				Vendor:     "opencode",
				ModelUsed:  req.Model,
				Attempts: []provider.Attempt{
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
	if got := streamObj.closedCount(); got != 1 {
		t.Errorf("Stream.Close() calls = %d, want 1", got)
	}

	got := captured.last(t)
	if got.Method != "chat.completions.stream" {
		t.Errorf("Method = %q, want chat.completions.stream", got.Method)
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
	streamObj := &fakeStream{
		recvErr: &provider.Error{Kind: provider.KindUpstream, Message: "boom mid-stream"},
		summary: &provider.Summary{},
	}
	r := &fakeRouter{
		buildStreamResult: func(req *provider.Request) (*router.RouteResult, error) {
			return &router.RouteResult{
				Stream:     streamObj,
				FirstEvent: &provider.Event{Choices: []provider.ChoiceDelta{{Delta: provider.Delta{Content: "partial"}}}},
				Vendor:     "opencode",
				ModelUsed:  req.Model,
				Attempts: []provider.Attempt{
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
	if got.ErrorKind != provider.KindUpstream {
		t.Errorf("rec.ErrorKind = %q, want upstream", got.ErrorKind)
	}
	if len(got.Attempts) != 1 || got.Attempts[0].ErrorKind != provider.KindUpstream {
		t.Errorf("Attempts[0].ErrorKind not propagated: %+v", got.Attempts)
	}
}

func TestHandler_Stream_IdleTimeoutSendsError(t *testing.T) {
	captured, recorder := newCaptureRecorder()
	streamObj := &fakeStream{
		recvDelay: 50 * time.Millisecond,
		summary:   &provider.Summary{},
	}
	r := &fakeRouter{
		buildStreamResult: func(req *provider.Request) (*router.RouteResult, error) {
			return &router.RouteResult{
				Stream:    streamObj,
				Vendor:    "opencode",
				ModelUsed: req.Model,
				Attempts: []provider.Attempt{
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
	if got.ErrorKind != provider.KindTimeout {
		t.Errorf("rec.ErrorKind = %q, want timeout", got.ErrorKind)
	}
	if len(got.Attempts) != 1 || got.Attempts[0].ErrorKind != provider.KindTimeout {
		t.Errorf("Attempts[0].ErrorKind not propagated: %+v", got.Attempts)
	}
	if streamObj.closedCount() == 0 {
		t.Errorf("Stream.Close() calls = 0, want at least 1")
	}
}

func TestHandler_Stream_RequestTimeoutSendsError(t *testing.T) {
	captured, recorder := newCaptureRecorder()
	streamObj := &fakeStream{
		recvDelay: 50 * time.Millisecond,
		summary:   &provider.Summary{},
	}
	r := &fakeRouter{
		buildStreamResult: func(req *provider.Request) (*router.RouteResult, error) {
			return &router.RouteResult{
				Stream:    streamObj,
				Vendor:    "opencode",
				ModelUsed: req.Model,
				Attempts: []provider.Attempt{
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
	if got.ErrorKind != provider.KindTimeout {
		t.Errorf("rec.ErrorKind = %q, want timeout", got.ErrorKind)
	}
	if len(got.Attempts) != 1 || got.Attempts[0].ErrorKind != provider.KindTimeout {
		t.Errorf("Attempts[0].ErrorKind not propagated: %+v", got.Attempts)
	}
}

// TestHandler_Stream_ClientDisconnect_MidStream simulates a client that
// hangs up after the first SSE frame. The handler must (a) record the
// terminal state as KindClientClosed in audit and (b) stop draining the
// upstream stream — leaving later events un-consumed.
func TestHandler_Stream_ClientDisconnect_MidStream(t *testing.T) {
	captured, recorder := newCaptureRecorder()
	streamObj := &fakeStream{
		events: []*provider.Event{
			{Choices: []provider.ChoiceDelta{{Delta: provider.Delta{Content: " two"}}}},
			{Choices: []provider.ChoiceDelta{{Delta: provider.Delta{Content: " three"}}}},
		},
		summary: &provider.Summary{},
	}
	r := &fakeRouter{
		buildStreamResult: func(req *provider.Request) (*router.RouteResult, error) {
			return &router.RouteResult{
				Stream:     streamObj,
				FirstEvent: &provider.Event{Choices: []provider.ChoiceDelta{{Delta: provider.Delta{Content: "one"}}}},
				Vendor:     "opencode",
				ModelUsed:  req.Model,
				Attempts: []provider.Attempt{
					{Vendor: "opencode", Model: req.Model, StartedAt: time.Now()},
				},
			}, nil
		},
	}
	h := NewHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)), recorder, HandlerConfig{})

	body := `{"model":"deepseek-v4-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	// Allow one frame (FirstEvent) through; next Send call fails.
	w := newDisconnectAfterN(1)
	h.ServeHTTP(w, req)

	got := captured.last(t)
	if got.ErrorKind != provider.KindClientClosed {
		t.Fatalf("ErrorKind = %q, want client_closed", got.ErrorKind)
	}
	// Loop runs exactly one Recv: receives event[0], marshals it, Send fails,
	// handler bails. event[1] must remain in the buffer.
	if streamObj.cursor != 1 {
		t.Errorf("stream cursor = %d, want 1 (one Recv call before bail-out)", streamObj.cursor)
	}
	if streamObj.closedCount() == 0 {
		t.Errorf("Stream.Close() not called (defer must run)")
	}
}

// TestHandler_Stream_ClientDisconnect_OnDone covers the EOF success
// path: stream drains cleanly but the [DONE] sentinel write fails.
// Audit must still record client_closed — the wire handshake didn't
// complete even though upstream finished cleanly.
func TestHandler_Stream_ClientDisconnect_OnDone(t *testing.T) {
	captured, recorder := newCaptureRecorder()
	streamObj := &fakeStream{
		// no events: Recv returns io.EOF immediately
		summary: &provider.Summary{},
	}
	r := &fakeRouter{
		buildStreamResult: func(req *provider.Request) (*router.RouteResult, error) {
			return &router.RouteResult{
				Stream:     streamObj,
				FirstEvent: &provider.Event{Choices: []provider.ChoiceDelta{{Delta: provider.Delta{Content: "only"}}}},
				Vendor:     "opencode",
				ModelUsed:  req.Model,
				Attempts: []provider.Attempt{
					{Vendor: "opencode", Model: req.Model, StartedAt: time.Now()},
				},
			}, nil
		},
	}
	h := NewHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)), recorder, HandlerConfig{})

	body := `{"model":"deepseek-v4-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	// Allow FirstEvent through (1 write); next Write (the [DONE]) fails.
	w := newDisconnectAfterN(1)
	h.ServeHTTP(w, req)

	got := captured.last(t)
	if got.ErrorKind != provider.KindClientClosed {
		t.Errorf("ErrorKind = %q, want client_closed (SendDone failed)", got.ErrorKind)
	}
}

// TestHandler_Stream_ClientDisconnect_OnFirstEvent covers the path where
// the very first SSE write fails — handler should bail without entering
// the Recv loop.
func TestHandler_Stream_ClientDisconnect_OnFirstEvent(t *testing.T) {
	captured, recorder := newCaptureRecorder()
	streamObj := &fakeStream{
		events: []*provider.Event{
			{Choices: []provider.ChoiceDelta{{Delta: provider.Delta{Content: "later"}}}},
		},
		summary: &provider.Summary{},
	}
	r := &fakeRouter{
		buildStreamResult: func(req *provider.Request) (*router.RouteResult, error) {
			return &router.RouteResult{
				Stream:     streamObj,
				FirstEvent: &provider.Event{Choices: []provider.ChoiceDelta{{Delta: provider.Delta{Content: "first"}}}},
				Vendor:     "opencode",
				ModelUsed:  req.Model,
				Attempts: []provider.Attempt{
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
	if got.ErrorKind != provider.KindClientClosed {
		t.Fatalf("ErrorKind = %q, want client_closed", got.ErrorKind)
	}
	if streamObj.cursor != 0 {
		t.Errorf("stream cursor = %d, want 0 (Recv loop must not run when FirstEvent send fails)", streamObj.cursor)
	}
}

// TestHandler_NonStream_ClientDisconnect verifies the JSON response
// path also tags audit when the client write fails. StatusCode stays 200
// (already on the wire), but ErrorKind reveals the terminal state.
func TestHandler_NonStream_ClientDisconnect(t *testing.T) {
	captured, recorder := newCaptureRecorder()
	r := &fakeRouter{
		buildResult: func(req *provider.Request) *router.RouteResult {
			return &router.RouteResult{
				Response: &provider.Response{
					Model:   req.Model,
					Choices: []provider.Choice{{Index: 0, Message: provider.Message{Role: "assistant", Content: "ok"}}},
				},
				Vendor:    "opencode",
				ModelUsed: req.Model,
				Attempts: []provider.Attempt{
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
	if got.ErrorKind != provider.KindClientClosed {
		t.Errorf("ErrorKind = %q, want client_closed", got.ErrorKind)
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

func TestHandler_Stream_PreStreamRouterError(t *testing.T) {
	captured, recorder := newCaptureRecorder()
	r := &fakeRouter{
		buildStreamResult: func(req *provider.Request) (*router.RouteResult, error) {
			return &router.RouteResult{
				Vendor:    "opencode",
				ModelUsed: req.Model,
				Attempts: []provider.Attempt{
					{Vendor: "opencode", Model: req.Model, ErrorKind: provider.KindAuth, StatusCode: 401, StartedAt: time.Now()},
				},
			}, &provider.Error{Kind: provider.KindAuth, Message: "no key"}
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
	if got.ErrorKind != provider.KindAuth {
		t.Errorf("rec.ErrorKind = %q, want auth", got.ErrorKind)
	}
	if got.StatusCode != http.StatusUnauthorized {
		t.Errorf("rec.StatusCode = %d, want 401", got.StatusCode)
	}
	if len(got.Attempts) != 1 || got.Attempts[0].ErrorKind != provider.KindAuth {
		t.Errorf("Attempts[0] = %+v, want auth attempt", got.Attempts)
	}
}

// fakeRouter implements ChatRouter for handler tests. buildResult /
// buildStreamResult let each test case shape the RouteResult —
// including pre-populated Attempts — so we exercise the audit-copy
// path without spinning up a real Router.
type fakeRouter struct {
	vendor            string
	buildResult       func(req *provider.Request) *router.RouteResult
	buildStreamResult func(req *provider.Request) (*router.RouteResult, error)
}

func (f *fakeRouter) Complete(_ context.Context, req *provider.Request) (*router.RouteResult, error) {
	return f.buildResult(req), nil
}

func (f *fakeRouter) CompleteStream(_ context.Context, req *provider.Request) (*router.RouteResult, error) {
	if f.buildStreamResult != nil {
		return f.buildStreamResult(req)
	}
	return &router.RouteResult{}, &provider.Error{Kind: provider.KindUpstream, Message: "stream not implemented in this fake"}
}

// fakeStream returns events in order, then optionally yields recvErr
// on the next Recv (or io.EOF if recvErr is nil).
type fakeStream struct {
	mu        sync.Mutex
	closeOnce sync.Once
	done      chan struct{}
	events    []*provider.Event
	cursor    int
	recvErr   error
	recvDelay time.Duration
	summary   *provider.Summary
	closed    int
}

func (s *fakeStream) Recv() (*provider.Event, error) {
	if s.recvDelay > 0 {
		select {
		case <-time.After(s.recvDelay):
		case <-s.doneChan():
			return nil, io.EOF
		}
	}
	if s.cursor < len(s.events) {
		e := s.events[s.cursor]
		s.cursor++
		return e, nil
	}
	if s.recvErr != nil {
		return nil, s.recvErr
	}
	return nil, io.EOF
}

func (s *fakeStream) Close() error {
	done := s.doneChan()
	s.closeOnce.Do(func() { close(done) })
	s.mu.Lock()
	s.closed++
	s.mu.Unlock()
	return nil
}

func (s *fakeStream) Summary() *provider.Summary { return s.summary }

func (s *fakeStream) doneChan() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done == nil {
		s.done = make(chan struct{})
	}
	return s.done
}

func (s *fakeStream) closedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
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

func (s *stubbornStream) Recv() (*provider.Event, error) {
	<-s.block
	return nil, io.EOF
}

func (s *stubbornStream) Close() error {
	s.closeCalled++
	return nil
}

func (s *stubbornStream) Summary() *provider.Summary { return &provider.Summary{} }

func (s *stubbornStream) release() { close(s.block) }

func TestRecvWithIdleTimeout_BoundedDrainOnContextCancel(t *testing.T) {
	prev := provider.CloseGrace
	provider.CloseGrace = 50 * time.Millisecond
	defer func() { provider.CloseGrace = prev }()

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
	prev := provider.CloseGrace
	provider.CloseGrace = 50 * time.Millisecond
	defer func() { provider.CloseGrace = prev }()

	s := newStubbornStream()
	defer s.release()

	start := time.Now()
	_, err := recvWithIdleTimeout(context.Background(), s, 20*time.Millisecond)
	elapsed := time.Since(start)

	// Idle timer fires (~20ms) → Close → 50ms grace → return. Total ~70ms.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("recvWithIdleTimeout returned in %v, want < 500ms", elapsed)
	}
	var perr *provider.Error
	if !errors.As(err, &perr) || perr.Kind != provider.KindTimeout {
		t.Errorf("err = %v, want KindTimeout provider.Error", err)
	}
	if s.closeCalled == 0 {
		t.Errorf("Stream.Close() not invoked on idle timeout")
	}
}
