package response

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCountingWriter_TracksStatusAndBytes(t *testing.T) {
	rec := httptest.NewRecorder()
	cw := NewCountingWriter(rec)

	n, err := cw.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write err = %v", err)
	}
	if n != 5 {
		t.Fatalf("Write n = %d, want 5", n)
	}
	if cw.Status() != http.StatusOK {
		t.Errorf("Status = %d, want 200", cw.Status())
	}
	if cw.Bytes() != 5 {
		t.Errorf("Bytes = %d, want 5", cw.Bytes())
	}
	if !cw.WroteHeader() {
		t.Error("WroteHeader = false, want true")
	}
}

func TestCountingWriter_WriteHeaderOnlyOnce(t *testing.T) {
	rec := httptest.NewRecorder()
	cw := NewCountingWriter(rec)

	cw.WriteHeader(http.StatusAccepted)
	cw.WriteHeader(http.StatusInternalServerError)

	if cw.Status() != http.StatusAccepted {
		t.Errorf("Status = %d, want 202", cw.Status())
	}
	if rec.Code != http.StatusAccepted {
		t.Errorf("rec.Code = %d, want 202", rec.Code)
	}
}

// passThroughWrapper mimics a downstream middleware that wraps the
// upstream ResponseWriter without exposing WroteHeader. It implements
// Unwrap so HeadersWritten can still reach the CountingWriter beneath.
type passThroughWrapper struct {
	http.ResponseWriter
}

func (p *passThroughWrapper) Unwrap() http.ResponseWriter { return p.ResponseWriter }

func TestHeadersWritten_DirectCountingWriter(t *testing.T) {
	cw := NewCountingWriter(httptest.NewRecorder())
	if HeadersWritten(cw) {
		t.Error("HeadersWritten = true before any write, want false")
	}
	cw.WriteHeader(http.StatusOK)
	if !HeadersWritten(cw) {
		t.Error("HeadersWritten = false after WriteHeader, want true")
	}
}

func TestHeadersWritten_WrappedCountingWriter(t *testing.T) {
	cw := NewCountingWriter(httptest.NewRecorder())
	wrapped := &passThroughWrapper{ResponseWriter: cw}
	if HeadersWritten(wrapped) {
		t.Error("HeadersWritten through wrapper = true before write, want false")
	}
	cw.WriteHeader(http.StatusOK)
	if !HeadersWritten(wrapped) {
		t.Error("HeadersWritten through wrapper = false after write, want true")
	}
}

func TestHeadersWritten_NoSignal(t *testing.T) {
	// A bare ResponseWriter without WroteHeader and without Unwrap.
	// The current convention is to treat that as "headers not written" —
	// the rare case is recoverPanic before any wrap was installed, where
	// it is still safe to write the JSON envelope.
	if HeadersWritten(httptest.NewRecorder()) {
		t.Error("HeadersWritten on bare recorder = true, want false")
	}
}
