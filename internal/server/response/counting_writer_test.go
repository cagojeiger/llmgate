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
