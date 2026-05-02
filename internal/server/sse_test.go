package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"llmgate/internal/provider"
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

	sw.SendError(&provider.Error{Kind: provider.KindUpstream, Message: "boom"})

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
