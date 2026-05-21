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
	"time"

	"llmgate/internal/domain/consumers"
	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/routing"
	"llmgate/internal/domain/telemetry"
	"llmgate/internal/platform/config"
	httpauth "llmgate/internal/platform/http/auth"
	httpchat "llmgate/internal/platform/http/chat"
	httpprobe "llmgate/internal/platform/http/probe"
)

// recordingRecorder captures every emitted audit event so tests can
// assert on the audit-always property without standing up a real sink.
type recordingRecorder struct {
	records []telemetry.AuditEvent
}

func (r *recordingRecorder) captureAudit(_ context.Context, rec *telemetry.AuditEvent) {
	r.records = append(r.records, *rec)
}
func (r *recordingRecorder) Close() error { return nil }
func (r *recordingRecorder) Emit(ctx context.Context, event telemetry.Event) {
	rec, ok := event.(*telemetry.AuditEvent)
	if !ok {
		return
	}
	r.captureAudit(ctx, rec)
}

// stubService implements ChatService without doing any real work — the
// handler tests in this file exercise auth, not routing, so a stub
// keeps test isolation tight. Complete is what serveComplete calls;
// stream is unused in these tests.
type stubService struct {
	resp     *routing.RouteResult
	err      error
	complete func(context.Context, *llmtypes.Request) (*routing.RouteResult, error)
}

func (s *stubService) Complete(ctx context.Context, req *llmtypes.Request) (*routing.RouteResult, error) {
	if s.complete != nil {
		return s.complete(ctx, req)
	}
	return s.resp, s.err
}
func (s *stubService) CompleteStream(context.Context, *llmtypes.Request) (*routing.RouteResult, error) {
	return s.resp, s.err
}

// writeStoreYAML drops one consumer yaml into a temp dir and loads it,
// returning the live store and the raw key the operator would issue.
func writeStoreYAML(t *testing.T, name, rawKey string, allowedAliases ...string) *consumers.Store {
	t.Helper()
	dir := t.TempDir()
	sum := sha256.Sum256([]byte(rawKey))
	yaml := "name: " + name + "\nkey_hashes:\n  - sha256:" + hex.EncodeToString(sum[:]) + "\n"
	if len(allowedAliases) > 0 {
		yaml += "allowed_aliases:\n"
		for _, alias := range allowedAliases {
			yaml += "  - " + alias + "\n"
		}
	}
	path := filepath.Join(dir, name+".yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write consumer yaml: %v", err)
	}
	store, err := consumers.LoadDir(dir)
	if err != nil {
		t.Fatalf("load store: %v", err)
	}
	return store
}

func TestClassifyAuth_NoHeader(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	got := httpauth.Classify(r, nil)
	if got.AuthError != telemetry.AuthErrorMissing {
		t.Fatalf("AuthError = %q, want %q", got.AuthError, telemetry.AuthErrorMissing)
	}
	if got.Name != "" || got.KeyID != "" {
		t.Errorf("consumer info populated on missing header: %+v", got)
	}
}

func TestClassifyAuth_BadFormat(t *testing.T) {
	cases := map[string]string{
		"raw-key":         "raw-key",
		"basic":           "Basic abcdef",
		"bearer-no-token": "Bearer ",
		"bearer-spaces":   "Bearer    ",
		"bearer-no-space": "Bearerabcdef",
	}
	for label, header := range cases {
		t.Run(label, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			r.Header.Set("Authorization", header)
			got := httpauth.Classify(r, nil)
			if got.AuthError != telemetry.AuthErrorFormat {
				t.Fatalf("AuthError = %q, want %q (header=%q)", got.AuthError, telemetry.AuthErrorFormat, header)
			}
		})
	}
}

func TestClassifyAuth_UnknownKey(t *testing.T) {
	store := writeStoreYAML(t, "alpha", "rotated-out")
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer not-issued")
	got := httpauth.Classify(r, store)
	if got.AuthError != telemetry.AuthErrorUnknown {
		t.Fatalf("AuthError = %q, want %q", got.AuthError, telemetry.AuthErrorUnknown)
	}
	if got.Name != "" {
		t.Errorf("Name = %q, want empty on unknown key", got.Name)
	}
}

func TestClassifyAuth_KnownKey(t *testing.T) {
	store := writeStoreYAML(t, "alpha", "good-key", "cheap", "worker")
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer good-key")
	got := httpauth.Classify(r, store)
	if got.AuthError != "" {
		t.Fatalf("AuthError = %q, want empty (success)", got.AuthError)
	}
	if got.Name != "alpha" {
		t.Errorf("Name = %q, want alpha", got.Name)
	}
	if len(got.KeyID) != 8 {
		t.Errorf("KeyID = %q (len %d), want 8 hex chars", got.KeyID, len(got.KeyID))
	}
	if strings.Join(got.AllowedAliases, ",") != "cheap,worker" {
		t.Errorf("AllowedAliases = %#v, want [cheap worker]", got.AllowedAliases)
	}
}

func TestClassifyAuth_BearerCaseInsensitive(t *testing.T) {
	store := writeStoreYAML(t, "alpha", "good-key")
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "bearer good-key")
	got := httpauth.Classify(r, store)
	if got.Name != "alpha" {
		t.Errorf("lowercase 'bearer' should be accepted; got %+v", got)
	}
}

// authChain mirrors the production middleware order so unit tests that
// exercise just the auth surface still get the *httpauth.ConsumerInfo pointer
// allocated by httpauth.ContextMiddleware.
func authChain(store *consumers.Store, next http.Handler) http.Handler {
	return httpauth.ContextMiddleware(httpauth.Middleware(store)(next))
}

func TestAuthMiddleware_StashesContext(t *testing.T) {
	store := writeStoreYAML(t, "alpha", "good-key")
	var captured httpauth.ConsumerInfo
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = httpauth.FromContext(r.Context())
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
		got := httpauth.FromContext(r.Context())
		if got.AuthError != telemetry.AuthErrorUnknown {
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

	stub := &stubService{resp: &routing.RouteResult{
		Response: &llmtypes.Response{
			ID:      "resp-1",
			Object:  "chat.completion",
			Model:   "claude-x",
			Choices: []llmtypes.Choice{{Index: 0, Message: llmtypes.Message{Role: "assistant", Content: "ok"}}},
			Usage:   &llmtypes.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		},
		Vendor:    "anthropic",
		ModelUsed: "claude-x",
	}}
	handler := httpchat.NewHandler(stub, logger, rec, httpchat.HandlerConfig{RequestTimeout: 30 * time.Second})
	srv := New(&config.Server{Addr: ":0"}, logger, handler, store, httpprobe.NewState())
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	type call struct {
		label         string
		header        string
		wantStatus    int
		wantClient    string
		wantKind      llmtypes.ErrorKind
		wantAuthError telemetry.AuthError
	}
	cases := []call{
		{"no-auth", "", http.StatusUnauthorized, "", llmtypes.KindAuth, "missing"},
		{"bad-format", "Token foo", http.StatusUnauthorized, "", llmtypes.KindAuth, "format"},
		{"unknown-key", "Bearer wrong", http.StatusUnauthorized, "", llmtypes.KindAuth, "unknown"},
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
		if got.ConsumerName != c.wantClient {
			t.Errorf("[%s] consumer_name = %q, want %q", c.label, got.ConsumerName, c.wantClient)
		}
		if got.Kind != c.wantKind {
			t.Errorf("[%s] error_kind = %q, want %q", c.label, got.Kind, c.wantKind)
		}
		if got.AuthError != c.wantAuthError {
			t.Errorf("[%s] auth_error = %q, want %q", c.label, got.AuthError, c.wantAuthError)
		}
		if c.wantAuthError != "" {
			if got.AuthResult != telemetry.AuthResultFailure ||
				got.PolicyResult != telemetry.PolicyResultDenied ||
				got.DenyReason != telemetry.DenyReasonAuth {
				t.Errorf(
					"[%s] auth decision = %q/%q/%q, want failure/denied/auth",
					c.label,
					got.AuthResult,
					got.PolicyResult,
					got.DenyReason,
				)
			}
		} else if got.AuthResult != telemetry.AuthResultSuccess || got.PolicyResult != telemetry.PolicyResultAllowed {
			t.Errorf("[%s] success decision = %q/%q, want success/allowed", c.label, got.AuthResult, got.PolicyResult)
		}
		if c.wantClient != "" && got.ConsumerKeyID == "" {
			t.Errorf("[%s] consumer_key_id empty on success record", c.label)
		}
	}

	// Access log must surface caller identity on the success line and the
	// specific failure mode on the auth-failure line, since the wire 401
	// alone cannot distinguish missing vs format vs unknown.
	logged := logBuf.String()
	if !strings.Contains(logged, `"consumer_name":"alpha"`) {
		t.Errorf("access log missing consumer_name field for success request: %s", logged)
	}
	for _, want := range []string{`"auth_error":"missing"`, `"auth_error":"format"`, `"auth_error":"unknown"`} {
		if !strings.Contains(logged, want) {
			t.Errorf("access log missing %s: %s", want, logged)
		}
	}
}

func TestServer_AllowedAliasesRejectDisallowedModel(t *testing.T) {
	store := writeStoreYAML(t, "alpha", "good-key", "cheap")
	rec := &recordingRecorder{}
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	stubCalled := false
	stub := &stubService{resp: &routing.RouteResult{
		Response: &llmtypes.Response{ID: "resp-1", Object: "chat.completion", Model: "smart"},
	}}
	stub.complete = func(context.Context, *llmtypes.Request) (*routing.RouteResult, error) {
		stubCalled = true
		return stub.resp, nil
	}
	handler := httpchat.NewHandler(stub, logger, rec, httpchat.HandlerConfig{RequestTimeout: 30 * time.Second})
	srv := New(&config.Server{Addr: ":0"}, logger, handler, store, httpprobe.NewState())
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	body := `{"model":"smart","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer good-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if stubCalled {
		t.Fatal("service called for disallowed model")
	}
	if len(rec.records) != 1 {
		t.Fatalf("records = %d, want 1", len(rec.records))
	}
	got := rec.records[0]
	if got.ConsumerName != "alpha" {
		t.Fatalf("audit identity = %q, want alpha", got.ConsumerName)
	}
	if got.Kind != llmtypes.KindForbidden || got.StatusCode != http.StatusForbidden {
		t.Fatalf("audit kind/status = %q/%d, want forbidden/403", got.Kind, got.StatusCode)
	}
	if got.PolicyResult != telemetry.PolicyResultDenied || got.DenyReason != telemetry.DenyReasonModelNotAllowed {
		t.Fatalf("audit policy decision = %q/%q, want denied/model_not_allowed", got.PolicyResult, got.DenyReason)
	}
	if got.ResourceType != "llm_model" || got.ResourceID != "smart" {
		t.Fatalf("audit resource = %q/%q, want llm_model/smart", got.ResourceType, got.ResourceID)
	}
	if !strings.Contains(logBuf.String(), `"consumer_name":"alpha"`) {
		t.Fatalf("access log missing consumer_name: %s", logBuf.String())
	}
}

func TestServer_HealthzPublic(t *testing.T) {
	// Smoke-only: probes are unauthenticated even when consumers are
	// registered. Detailed probe-state coverage lives in probe_test.go.
	store := writeStoreYAML(t, "alpha", "good-key")
	handler := httpchat.NewHandler(&stubService{}, slog.Default(), &recordingRecorder{}, httpchat.HandlerConfig{RequestTimeout: 30 * time.Second})
	srv := New(&config.Server{Addr: ":0"}, slog.Default(), handler, store, httpprobe.NewState())
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
