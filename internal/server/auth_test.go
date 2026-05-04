package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"llmgate/internal/audit"
	"llmgate/internal/clients"
	"llmgate/internal/config"
	"llmgate/internal/provider"
	"llmgate/internal/dispatch"
)

// recordingRecorder captures every emitted audit Record so tests can
// assert on the audit-always property without standing up a real sink.
type recordingRecorder struct {
	records []audit.Record
}

func (r *recordingRecorder) Record(_ context.Context, rec *audit.Record) {
	r.records = append(r.records, *rec)
}
func (r *recordingRecorder) Close() error { return nil }

// stubDispatcher implements ChatDispatcher without doing any real work — the
// handler tests in this file exercise auth, not routing, so a stub
// keeps test isolation tight. Complete is what serveComplete calls;
// stream is unused in these tests.
type stubDispatcher struct {
	resp *dispatch.Result
	err  error
}

func (s *stubDispatcher) Complete(context.Context, *provider.Request) (*dispatch.Result, error) {
	return s.resp, s.err
}
func (s *stubDispatcher) CompleteStream(context.Context, *provider.Request) (*dispatch.Result, error) {
	return s.resp, s.err
}

// writeStoreYAML drops one client yaml into a temp dir and loads it,
// returning the live store and the raw key the operator would issue.
func writeStoreYAML(t *testing.T, name, rawKey string) *clients.Store {
	t.Helper()
	dir := t.TempDir()
	sum := sha256.Sum256([]byte(rawKey))
	yaml := "name: " + name + "\nkey_hashes:\n  - sha256:" + hex.EncodeToString(sum[:]) + "\n"
	path := filepath.Join(dir, name+".yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write client yaml: %v", err)
	}
	store, err := clients.LoadDir(dir)
	if err != nil {
		t.Fatalf("load store: %v", err)
	}
	return store
}

func TestClassifyAuth_NoHeader(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	got := classifyAuth(r, nil)
	if got.AuthError != AuthErrorMissing {
		t.Fatalf("AuthError = %q, want %q", got.AuthError, AuthErrorMissing)
	}
	if got.Name != "" || got.KeyID != "" {
		t.Errorf("client info populated on missing header: %+v", got)
	}
}

func TestClassifyAuth_BadFormat(t *testing.T) {
	cases := map[string]string{
		"raw-key":          "raw-key",
		"basic":            "Basic abcdef",
		"bearer-no-token":  "Bearer ",
		"bearer-spaces":    "Bearer    ",
		"bearer-no-space":  "Bearerabcdef",
	}
	for label, header := range cases {
		t.Run(label, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			r.Header.Set("Authorization", header)
			got := classifyAuth(r, nil)
			if got.AuthError != AuthErrorFormat {
				t.Fatalf("AuthError = %q, want %q (header=%q)", got.AuthError, AuthErrorFormat, header)
			}
		})
	}
}

func TestClassifyAuth_UnknownKey(t *testing.T) {
	store := writeStoreYAML(t, "alpha", "rotated-out")
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer not-issued")
	got := classifyAuth(r, store)
	if got.AuthError != AuthErrorUnknown {
		t.Fatalf("AuthError = %q, want %q", got.AuthError, AuthErrorUnknown)
	}
	if got.Name != "" {
		t.Errorf("Name = %q, want empty on unknown key", got.Name)
	}
}

func TestClassifyAuth_KnownKey(t *testing.T) {
	store := writeStoreYAML(t, "alpha", "good-key")
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer good-key")
	got := classifyAuth(r, store)
	if got.AuthError != "" {
		t.Fatalf("AuthError = %q, want empty (success)", got.AuthError)
	}
	if got.Name != "alpha" {
		t.Errorf("Name = %q, want alpha", got.Name)
	}
	if len(got.KeyID) != 8 {
		t.Errorf("KeyID = %q (len %d), want 8 hex chars", got.KeyID, len(got.KeyID))
	}
}

func TestClassifyAuth_BearerCaseInsensitive(t *testing.T) {
	store := writeStoreYAML(t, "alpha", "good-key")
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "bearer good-key")
	got := classifyAuth(r, store)
	if got.Name != "alpha" {
		t.Errorf("lowercase 'bearer' should be accepted; got %+v", got)
	}
}

// authChain mirrors the production middleware order so unit tests that
// exercise just the auth surface still get the *ClientInfo pointer
// allocated by clientContextMiddleware.
func authChain(store *clients.Store, next http.Handler) http.Handler {
	return clientContextMiddleware(authMiddleware(store)(next))
}

func TestAuthMiddleware_StashesContext(t *testing.T) {
	store := writeStoreYAML(t, "alpha", "good-key")
	var captured ClientInfo
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = ClientFromContext(r.Context())
	})
	srv := authChain(store, next)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer good-key")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)

	if captured.Name != "alpha" {
		t.Fatalf("ctx Name = %q, want alpha", captured.Name)
	}
}

func TestAuthMiddleware_AlwaysCallsNext(t *testing.T) {
	// Audit-always: a failed Authorization must still reach the handler
	// so the handler's defer can emit a record.
	store := writeStoreYAML(t, "alpha", "good-key")
	called := false
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
		got := ClientFromContext(r.Context())
		if got.AuthError != AuthErrorUnknown {
			t.Errorf("ctx AuthError = %q, want unknown", got.AuthError)
		}
	})
	srv := authChain(store, next)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer wrong")
	srv.ServeHTTP(httptest.NewRecorder(), r)
	if !called {
		t.Fatal("middleware short-circuited; want next.ServeHTTP to run on auth failure too")
	}
}

func TestServer_AuthIntegration(t *testing.T) {
	store := writeStoreYAML(t, "alpha", "good-key")
	rec := &recordingRecorder{}
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	stub := &stubDispatcher{resp: &dispatch.Result{
		Response: &provider.Response{
			ID:      "resp-1",
			Object:  "chat.completion",
			Model:   "claude-x",
			Choices: []provider.Choice{{Index: 0, Message: provider.Message{Role: "assistant", Content: "ok"}}},
			Usage:   &provider.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		},
		Vendor:    "anthropic",
		ModelUsed: "claude-x",
	}}
	handler := NewHandler(stub, logger, rec, HandlerConfig{})
	srv := New(&config.Server{Addr: ":0"}, logger, handler, store, NewProbeState())
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	type call struct {
		label         string
		header        string
		wantStatus    int
		wantClient    string
		wantKind      provider.Kind
		wantAuthError string
	}
	cases := []call{
		{"no-auth", "", http.StatusUnauthorized, "", provider.KindAuth, "missing"},
		{"bad-format", "Token foo", http.StatusUnauthorized, "", provider.KindAuth, "format"},
		{"unknown-key", "Bearer wrong", http.StatusUnauthorized, "", provider.KindAuth, "unknown"},
		{"good-key", "Bearer good-key", http.StatusOK, "alpha", "", ""},
	}
	body := `{"model":"claude-x","messages":[{"role":"user","content":"hi"}]}`
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			if c.header != "" {
				req.Header.Set("Authorization", c.header)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != c.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, c.wantStatus)
			}
		})
	}

	// audit-always: every case (success or auth-fail) must produce one record.
	if len(rec.records) != len(cases) {
		t.Fatalf("records = %d, want %d (audit-always)", len(rec.records), len(cases))
	}
	for i, c := range cases {
		got := rec.records[i]
		if got.ClientName != c.wantClient {
			t.Errorf("[%s] client_name = %q, want %q", c.label, got.ClientName, c.wantClient)
		}
		if got.ErrorKind != c.wantKind {
			t.Errorf("[%s] error_kind = %q, want %q", c.label, got.ErrorKind, c.wantKind)
		}
		if got.AuthError != c.wantAuthError {
			t.Errorf("[%s] auth_error = %q, want %q", c.label, got.AuthError, c.wantAuthError)
		}
		if c.wantClient != "" && got.ClientKeyID == "" {
			t.Errorf("[%s] client_key_id empty on success record", c.label)
		}
	}

	// Access log must surface caller identity on the success line and the
	// specific failure mode on the auth-failure line, since the wire 401
	// alone cannot distinguish missing vs format vs unknown.
	logged := logBuf.String()
	if !strings.Contains(logged, `"client":"alpha"`) {
		t.Errorf("access log missing client field for success request: %s", logged)
	}
	for _, want := range []string{`"auth_error":"missing"`, `"auth_error":"format"`, `"auth_error":"unknown"`} {
		if !strings.Contains(logged, want) {
			t.Errorf("access log missing %s: %s", want, logged)
		}
	}
}

func TestServer_HealthzPublic(t *testing.T) {
	// Smoke-only: probes are unauthenticated even when clients are
	// registered. Detailed probe-state coverage lives in probe_test.go.
	store := writeStoreYAML(t, "alpha", "good-key")
	handler := NewHandler(&stubDispatcher{}, slog.Default(), &recordingRecorder{}, HandlerConfig{})
	srv := New(&config.Server{Addr: ":0"}, slog.Default(), handler, store, NewProbeState())
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200 (public)", resp.StatusCode)
	}
}
