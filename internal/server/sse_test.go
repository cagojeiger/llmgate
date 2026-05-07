package server

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llmgate/internal/llmtypes"
)

func TestSSEWriter_WriteHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	flusher := &stubFlusher{}
	sw := newSSEWriter(rec, flusher)

	sw.WriteHeaders()

	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
	if got := rec.Header().Get("Connection"); got != "keep-alive" {
		t.Errorf("Connection = %q, want keep-alive", got)
	}
	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", got)
	}
	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if flusher.calls != 1 {
		t.Errorf("flush calls = %d, want 1 (header flush)", flusher.calls)
	}
}

func TestSSEWriter_Send_FormatsAndCounts(t *testing.T) {
	rec := httptest.NewRecorder()
	flusher := &stubFlusher{}
	sw := newSSEWriter(rec, flusher)

	sw.Send([]byte(`{"a":1}`))
	sw.Send([]byte(`{"b":2}`))

	want := "data: {\"a\":1}\n\ndata: {\"b\":2}\n\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if int(sw.Bytes()) != len(want) {
		t.Errorf("Bytes() = %d, want %d (cumulative)", sw.Bytes(), len(want))
	}
	if flusher.calls != 2 {
		t.Errorf("flush calls = %d, want 2 (one per Send)", flusher.calls)
	}
}

func TestSSEWriter_SendDone(t *testing.T) {
	rec := httptest.NewRecorder()
	flusher := &stubFlusher{}
	sw := newSSEWriter(rec, flusher)

	sw.SendDone()

	const want = "data: [DONE]\n\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if int(sw.Bytes()) != len(want) {
		t.Errorf("Bytes() = %d, want %d", sw.Bytes(), len(want))
	}
}

func TestSSEWriter_SendError_EmbedsErrorPayload(t *testing.T) {
	rec := httptest.NewRecorder()
	flusher := &stubFlusher{}
	sw := newSSEWriter(rec, flusher)

	sw.SendError(&llmtypes.Error{ErrorKind: llmtypes.KindUpstream, Message: "boom"})

	body := rec.Body.String()
	if !strings.HasPrefix(body, "data: ") || !strings.HasSuffix(body, "\n\n") {
		t.Errorf("body = %q, want SSE-framed", body)
	}
	if !strings.Contains(body, `"type":"upstream"`) {
		t.Errorf("body = %q, want OpenAI-style envelope with type=upstream", body)
	}
	if !strings.Contains(body, `"message":"boom"`) {
		t.Errorf("body = %q, want message=boom", body)
	}
	if sw.Bytes() == 0 {
		t.Errorf("Bytes() = 0, want > 0")
	}
}

func TestSSEWriter_BytesAccumulatesAcrossOps(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := newSSEWriter(rec, &stubFlusher{})

	sw.Send([]byte("x"))
	mid := sw.Bytes()
	sw.SendDone()
	end := sw.Bytes()

	if mid >= end {
		t.Errorf("Bytes() did not grow across calls: mid=%d end=%d", mid, end)
	}
	if int(end) != rec.Body.Len() {
		t.Errorf("Bytes() = %d, body len = %d — must match", end, rec.Body.Len())
	}
}

// stubFlusher counts Flush calls so tests can assert that the writer
// flushed once per frame instead of buffering up.
type stubFlusher struct{ calls int }

func (s *stubFlusher) Flush() { s.calls++ }

// errResponseWriter simulates a client-disconnected ResponseWriter:
// every Write returns errBrokenPipe with zero bytes accepted. Header()
// works so callers that set headers before WriteHeader still pass.
type errResponseWriter struct {
	header http.Header
	err    error
}

func newErrResponseWriter(err error) *errResponseWriter {
	return &errResponseWriter{header: http.Header{}, err: err}
}

func (e *errResponseWriter) Header() http.Header        { return e.header }
func (e *errResponseWriter) Write([]byte) (int, error)  { return 0, e.err }
func (e *errResponseWriter) WriteHeader(statusCode int) {}

func TestSSEWriter_Send_PropagatesWriteError(t *testing.T) {
	target := errors.New("broken pipe")
	sw := newSSEWriter(newErrResponseWriter(target), &stubFlusher{})

	if err := sw.Send([]byte("x")); !errors.Is(err, target) {
		t.Fatalf("Send err = %v, want %v", err, target)
	}
	if sw.Bytes() != 0 {
		t.Errorf("Bytes() = %d, want 0 when writer rejected all bytes", sw.Bytes())
	}
}

func TestSSEWriter_SendError_PropagatesWriteError(t *testing.T) {
	target := errors.New("broken pipe")
	sw := newSSEWriter(newErrResponseWriter(target), &stubFlusher{})

	if err := sw.SendError(io.ErrUnexpectedEOF); !errors.Is(err, target) {
		t.Fatalf("SendError err = %v, want underlying %v", err, target)
	}
}

func TestSSEWriter_SendDone_PropagatesWriteError(t *testing.T) {
	target := errors.New("broken pipe")
	sw := newSSEWriter(newErrResponseWriter(target), &stubFlusher{})

	if err := sw.SendDone(); !errors.Is(err, target) {
		t.Fatalf("SendDone err = %v, want %v", err, target)
	}
}
