package response

import "net/http"

// CountingWriter tracks response status and accepted bytes for access logs.
type CountingWriter struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func NewCountingWriter(w http.ResponseWriter) *CountingWriter {
	return &CountingWriter{ResponseWriter: w, status: http.StatusOK}
}

func (w *CountingWriter) Status() int { return w.status }

func (w *CountingWriter) Bytes() int64 { return w.bytes }

func (w *CountingWriter) WroteHeader() bool { return w.wroteHeader }

func (w *CountingWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *CountingWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *CountingWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *CountingWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
