package server

import (
	"fmt"
	"net/http"
)

// sseWriter writes SSE frames and tracks response bytes for audit.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	bytes   int64
}

func newSSEWriter(w http.ResponseWriter, flusher http.Flusher) *sseWriter {
	return &sseWriter{w: w, flusher: flusher}
}

// WriteHeaders emits standard SSE headers and flushes them.
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

// SendError writes an error envelope as one SSE data frame.
func (s *sseWriter) SendError(err error) {
	_, _, payload := errorPayload(err)
	n, _ := fmt.Fprintf(s.w, "data: %s\n\n", payload)
	s.bytes += int64(n)
	s.flusher.Flush()
}

// SendDone writes the terminating [DONE] sentinel.
func (s *sseWriter) SendDone() {
	n, _ := s.w.Write([]byte("data: [DONE]\n\n"))
	s.bytes += int64(n)
	s.flusher.Flush()
}

// Bytes returns the running total of bytes written.
func (s *sseWriter) Bytes() int64 { return s.bytes }
