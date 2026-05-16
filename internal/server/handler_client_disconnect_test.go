package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llmgate/internal/llmrouter"
	"llmgate/internal/llmtypes"
	"llmgate/internal/providers/fake"
)

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
	h := newTestHandler(r, recorder, callSink, HandlerConfig{})

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
	h := newTestHandler(r, recorder, callSink, HandlerConfig{})

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
	h := newTestHandler(r, recorder, callSink, HandlerConfig{})

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
	h := newTestHandler(r, recorder, callSink, HandlerConfig{})

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
