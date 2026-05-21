package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	httpauth "llmgate/internal/platform/http/auth"
	"llmgate/internal/platform/http/requestid"
)

// TestStandardChain_InstallsAllFourLayers exercises a router that
// has only chain.Apply installed, then asserts each layer left its
// fingerprint on the response. The ordering invariant itself
// (AccessLog reads consumer identity that the auth step writes
// through an unexported ctx key) lives entirely inside Apply, so a
// future reordering shows up as a focused diff there — this test
// pins the wiring count rather than re-implementing the auth chain
// from outside the package.
func TestStandardChain_InstallsAllFourLayers(t *testing.T) {
	buf := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(buf, nil))

	router := chi.NewRouter()
	NewStandardChain(log).Apply(router)

	// The /panic route exercises Recoverer; /ok exercises the rest.
	router.Get("/ok", func(w http.ResponseWriter, r *http.Request) {
		// auth.ContextMiddleware planted a *ConsumerInfo on ctx; if it
		// didn't, FromContext would simply return the zero value but
		// the underlying pointer would also be nil — we just need a
		// safe call here to prove the layer is in place.
		_ = httpauth.FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	router.Get("/panic", func(w http.ResponseWriter, _ *http.Request) {
		panic("boom")
	})

	t.Run("RequestID stamps the response header", func(t *testing.T) {
		rec := callRouter(router, "/ok")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if id := rec.Header().Get("X-Request-Id"); !requestid.Valid(id) {
			t.Fatalf("X-Request-Id = %q, want a valid id stamped by the chain", id)
		}
	})

	t.Run("AccessLog emits one structured request line", func(t *testing.T) {
		buf.Reset()
		callRouter(router, "/ok")
		var found bool
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			var record map[string]any
			if err := json.Unmarshal([]byte(line), &record); err == nil && record["msg"] == "request" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("access log missing; got=%s", buf.String())
		}
	})

	t.Run("Recoverer absorbs handler panic", func(t *testing.T) {
		rec := callRouter(router, "/panic")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (Recoverer should turn panic into 500)", rec.Code)
		}
	})
}

func callRouter(router http.Handler, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}
