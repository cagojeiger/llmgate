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
	r := &fakeRouter{
		vendor: "opencode",
		buildResult: func(req *provider.Request) *provider.RouteResult {
			return &provider.RouteResult{
				Response: &provider.Response{
					Model:   req.Model,
					Choices: []provider.Choice{{Index: 0, Message: provider.Message{Role: "assistant", Content: "ok"}}},
				},
				Vendor:    "opencode",
				ModelUsed: req.Model,
				Attempts: []provider.Attempt{
					{Vendor: "opencode", Model: req.Model, StatusCode: 200, StartedAt: time.Now()},
				},
			}
		},
	}
	h := NewHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)), recorder)

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
	r := &fakeRouter{
		vendor: "opencode",
		buildResult: func(req *provider.Request) *provider.RouteResult {
			return &provider.RouteResult{
				Response: &provider.Response{
					Model:   "deepseek-v4-flash",
					Choices: []provider.Choice{{Index: 0, Message: provider.Message{Role: "assistant", Content: "ok"}}},
				},
				Vendor:    "opencode",
				ModelUsed: "deepseek-v4-flash",
				Attempts: []provider.Attempt{
					{Vendor: "opencode", Model: "deepseek-v4-pro", ErrorKind: provider.KindRateLimit, StatusCode: 429, StartedAt: time.Now()},
					{Vendor: "opencode", Model: "deepseek-v4-flash", StatusCode: 200, StartedAt: time.Now()},
				},
			}
		},
	}
	h := NewHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)), recorder)

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

func TestAdoptError_ProviderErrorMapsKindAndStatus(t *testing.T) {
	cases := []struct {
		name       string
		kind       provider.Kind
		wantStatus int
	}{
		{"auth", provider.KindAuth, http.StatusUnauthorized},
		{"rate_limit", provider.KindRateLimit, http.StatusTooManyRequests},
		{"bad_request", provider.KindBadRequest, http.StatusBadRequest},
		{"context_length", provider.KindContextLength, http.StatusBadRequest},
		{"upstream", provider.KindUpstream, http.StatusBadGateway},
		{"timeout", provider.KindTimeout, http.StatusBadGateway},
		{"unknown", provider.KindUnknown, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &audit.Record{}
			adoptError(rec, &provider.Error{Kind: tc.kind, Message: "x"})
			if rec.ErrorKind != tc.kind {
				t.Errorf("ErrorKind = %q, want %q", rec.ErrorKind, tc.kind)
			}
			if rec.StatusCode != tc.wantStatus {
				t.Errorf("StatusCode = %d, want %d", rec.StatusCode, tc.wantStatus)
			}
		})
	}
}

func TestAdoptError_NonProviderError_Falls500(t *testing.T) {
	rec := &audit.Record{}
	adoptError(rec, io.ErrUnexpectedEOF)
	if rec.ErrorKind != "" {
		t.Errorf("ErrorKind = %q, want empty (non-provider err shouldn't set kind)", rec.ErrorKind)
	}
	if rec.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want 500", rec.StatusCode)
	}
}

func TestAdoptStreamSummary_FinalizesAttemptAndRecord(t *testing.T) {
	started := time.Unix(1700000000, 0)
	now := started.Add(250 * time.Millisecond)
	rec := &audit.Record{
		Attempts: []provider.Attempt{
			{Vendor: "v", Model: "m", StartedAt: started},
		},
	}
	sum := &provider.Summary{
		Usage:      &provider.Usage{PromptTokens: 5, CompletionTokens: 7, TotalTokens: 12},
		VendorCost: `"0.001"`,
	}

	adoptStreamSummary(rec, sum, now)

	if rec.Usage == nil || rec.Usage.TotalTokens != 12 {
		t.Errorf("rec.Usage = %+v, want total=12", rec.Usage)
	}
	if rec.VendorCost != `"0.001"` {
		t.Errorf("rec.VendorCost = %q, want \"0.001\"", rec.VendorCost)
	}
	last := rec.Attempts[0]
	if last.DurationMS != 250 {
		t.Errorf("last.DurationMS = %d, want 250", last.DurationMS)
	}
	if last.Usage == nil || last.Usage.TotalTokens != 12 {
		t.Errorf("last.Usage = %+v, want total=12 propagated", last.Usage)
	}
	if last.VendorCost != `"0.001"` {
		t.Errorf("last.VendorCost = %q, want \"0.001\" propagated", last.VendorCost)
	}
}

func TestAdoptStreamSummary_PropagatesRecvErrorKindToAttempt(t *testing.T) {
	// Recv loop set rec.ErrorKind; the deferred summary sync must mirror
	// it onto the in-flight Attempt so audit logs stay symmetric with the
	// non-stream path.
	started := time.Unix(1700000000, 0)
	now := started.Add(100 * time.Millisecond)
	rec := &audit.Record{
		ErrorKind: provider.KindUpstream,
		Attempts: []provider.Attempt{
			{Vendor: "v", Model: "m", StartedAt: started},
		},
	}

	adoptStreamSummary(rec, nil, now)

	if rec.Attempts[0].ErrorKind != provider.KindUpstream {
		t.Errorf("attempt ErrorKind = %q, want upstream", rec.Attempts[0].ErrorKind)
	}
	if rec.Attempts[0].DurationMS != 100 {
		t.Errorf("DurationMS = %d, want 100", rec.Attempts[0].DurationMS)
	}
}

// fakeRouter implements ChatRouter for handler tests. buildResult lets
// each test case shape the RouteResult — including pre-populated
// Attempts so we exercise the audit-copy path without spinning up a
// real Router.
type fakeRouter struct {
	vendor      string
	buildResult func(req *provider.Request) *provider.RouteResult
}

func (f *fakeRouter) Complete(_ context.Context, req *provider.Request) (*provider.RouteResult, error) {
	return f.buildResult(req), nil
}

func (f *fakeRouter) CompleteStream(_ context.Context, _ *provider.Request) (*provider.RouteResult, error) {
	return &provider.RouteResult{}, &provider.Error{Kind: provider.KindUpstream, Message: "stream not implemented in this fake"}
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
