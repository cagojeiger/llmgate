package server

import (
	"fmt"
	"net/http"
)

// sseWriter wraps an http.ResponseWriter + Flusher for SSE writes,
// tracking bytes written so the handler can fold the count into the
// audit Record on the way out. Frame formatting (data: ...\n\n,
// terminating [DONE]) lives here so the handler stays a control-flow
// shell.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	bytes   int64
}

func newSSEWriter(w http.ResponseWriter, flusher http.Flusher) *sseWriter {
	return &sseWriter{w: w, flusher: flusher}
}

// WriteHeaders emits the standard SSE headers and a 200 status, then
// flushes so clients see the response start before the first chunk.
func (s *sseWriter) WriteHeaders() {
	s.w.Header().Set("Content-Type", "text/event-stream")
	s.w.Header().Set("Cache-Control", "no-cache")
	s.w.Header().Set("Connection", "keep-alive")
	s.w.Header().Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(http.StatusOK)
	s.flusher.Flush()
}

// Send writes one SSE data frame whose payload is the marshaled chunk.
func (s *sseWriter) Send(payload []byte) {
	n, _ := fmt.Fprintf(s.w, "data: %s\n\n", payload)
	s.bytes += int64(n)
	s.flusher.Flush()
}

// SendError writes the OpenAI-style error envelope as one SSE data
// frame. Mid-stream errors cannot change HTTP status (already 200), so
// they surface as event payloads.
func (s *sseWriter) SendError(err error) {
	_, _, payload := errorPayload(err)
	n, _ := fmt.Fprintf(s.w, "data: %s\n\n", payload)
	s.bytes += int64(n)
	s.flusher.Flush()
}

// SendDone writes the terminating [DONE] sentinel. This is the last
// frame any stream emits, success or error.
func (s *sseWriter) SendDone() {
	n, _ := s.w.Write([]byte("data: [DONE]\n\n"))
	s.bytes += int64(n)
	s.flusher.Flush()
}

// Bytes returns the running total of bytes written.
func (s *sseWriter) Bytes() int64 { return s.bytes }
