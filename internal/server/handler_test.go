package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"llmgate/internal/audit"
	"llmgate/internal/provider"
)

func TestHandler_SingleAttempt_RecordPopulated(t *testing.T) {
	rec, recorder := newCaptureRecorder()
	p := &recordingProvider{name: "opencode"}
	h := NewHandler(p, slog.New(slog.NewTextHandler(io.Discard, nil)), recorder)

	body := `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	got := rec.last(t)
	if got.ModelRequested != "deepseek-v4-flash" {
		t.Errorf("ModelRequested = %q, want deepseek-v4-flash", got.ModelRequested)
	}
	if got.Vendor != "opencode" || got.ModelUsed != "deepseek-v4-flash" {
		t.Errorf("Vendor/ModelUsed = %q/%q, want opencode/deepseek-v4-flash", got.Vendor, got.ModelUsed)
	}
	if len(got.Attempts) != 1 {
		t.Fatalf("len(Attempts) = %d, want 1", len(got.Attempts))
	}
	if got.Attempts[0].Vendor != "opencode" || got.Attempts[0].Model != "deepseek-v4-flash" {
		t.Errorf("attempt = %+v, want opencode/deepseek-v4-flash", got.Attempts[0])
	}
}

func TestHandler_FallbackChain_AttemptsRecorded(t *testing.T) {
	rec, recorder := newCaptureRecorder()
	// Provider records two attempts: first failed (rate_limit), second succeeded.
	p := &recordingProvider{
		name:           "opencode",
		extraAttempts:  []provider.Attempt{{Vendor: "opencode", Model: "deepseek-v4-pro", ErrorKind: provider.KindRateLimit, StatusCode: 429}},
		successAttempt: provider.Attempt{Vendor: "opencode", Model: "deepseek-v4-flash", StatusCode: 200},
	}
	h := NewHandler(p, slog.New(slog.NewTextHandler(io.Discard, nil)), recorder)

	body := `{"model":"coder","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	got := rec.last(t)
	if got.ModelRequested != "coder" {
		t.Errorf("ModelRequested = %q, want coder (alias)", got.ModelRequested)
	}
	if got.ModelUsed != "deepseek-v4-flash" {
		t.Errorf("ModelUsed = %q, want deepseek-v4-flash (last attempt)", got.ModelUsed)
	}
	if len(got.Attempts) != 2 {
		t.Fatalf("len(Attempts) = %d, want 2", len(got.Attempts))
	}
	if got.Attempts[0].ErrorKind != provider.KindRateLimit {
		t.Errorf("attempts[0].ErrorKind = %q, want rate_limit", got.Attempts[0].ErrorKind)
	}
}

// recordingProvider seeds extraAttempts (failed) before pushing
// successAttempt and returning success. Single-attempt callers leave
// extraAttempts nil and successAttempt with Model=req.Model.
type recordingProvider struct {
	name           string
	extraAttempts  []provider.Attempt
	successAttempt provider.Attempt
}

func (p *recordingProvider) Name() string { return p.name }

func (p *recordingProvider) Complete(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	for _, a := range p.extraAttempts {
		provider.RecordAttempt(ctx, a)
	}
	finish := p.successAttempt
	if finish.Vendor == "" {
		finish.Vendor = p.name
	}
	if finish.Model == "" {
		finish.Model = req.Model
	}
	if finish.StatusCode == 0 {
		finish.StatusCode = 200
	}
	if finish.StartedAt.IsZero() {
		finish.StartedAt = time.Now()
	}
	provider.RecordAttempt(ctx, finish)
	return &provider.Response{
		Model:   finish.Model,
		Choices: []provider.Choice{{Index: 0, Message: provider.Message{Role: "assistant", Content: "ok"}}},
	}, nil
}

func (p *recordingProvider) CompleteStream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	return nil, &provider.Error{Kind: provider.KindUpstream, Message: "stream not implemented in this fake"}
}

type captureRecorder struct {
	mu      sync.Mutex
	records []*audit.Record
}

func newCaptureRecorder() (*captureRecorder, audit.Recorder) {
	c := &captureRecorder{}
	return c, c
}

func (c *captureRecorder) Record(_ context.Context, r *audit.Record) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r)
}

func (c *captureRecorder) Close() error { return nil }

func (c *captureRecorder) last(t *testing.T) *audit.Record {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.records) == 0 {
		t.Fatalf("no records captured")
	}
	return c.records[len(c.records)-1]
}
