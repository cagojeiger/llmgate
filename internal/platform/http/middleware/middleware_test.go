package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"llmgate/internal/platform/http/requestid"
)

func TestRequestID_PropagatesValidHeader(t *testing.T) {
	var got string
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = requestid.FromContext(r.Context())
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-Id", "req-123")
	w := httptest.NewRecorder()

	RequestID(next).ServeHTTP(w, req)

	if got != "req-123" {
		t.Fatalf("context request id = %q, want req-123", got)
	}
	if header := w.Header().Get("X-Request-Id"); header != "req-123" {
		t.Fatalf("response request id = %q, want req-123", header)
	}
}

func TestRequestID_ReplacesInvalidHeader(t *testing.T) {
	var got string
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = requestid.FromContext(r.Context())
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-Id", "bad\nid")
	w := httptest.NewRecorder()

	RequestID(next).ServeHTTP(w, req)

	if got == "" || got == "bad\nid" || !requestid.Valid(got) {
		t.Fatalf("context request id = %q, want generated valid id", got)
	}
	if header := w.Header().Get("X-Request-Id"); header != got {
		t.Fatalf("response request id = %q, want generated context id %q", header, got)
	}
}
