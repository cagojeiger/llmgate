package provider

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"llmgate/internal/catalog"
)

func TestRouter_OpenAIOnly(t *testing.T) {
	var logs bytes.Buffer
	openAI := &fakeProvider{name: "openai"}

	router, err := NewRouter(stubCatalog(), map[string]AdapterFactory{
		"openai": func(ep *catalog.Endpoint) (Provider, error) {
			return openAI, nil
		},
	}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}
	if got := len(router.byModel); got != 12 {
		t.Fatalf("len(byModel) = %d, want 12", got)
	}
	if _, ok := router.byModel["minimax-m2.7"]; ok {
		t.Fatalf("anthropic model registered without anthropic factory")
	}
	if !strings.Contains(logs.String(), "no adapter for protocol") {
		t.Fatalf("logs = %q, want missing protocol warning", logs.String())
	}
}

func TestRouter_Both(t *testing.T) {
	router, err := NewRouter(stubCatalog(), map[string]AdapterFactory{
		"openai": func(ep *catalog.Endpoint) (Provider, error) {
			return &fakeProvider{name: "openai"}, nil
		},
		"anthropic": func(ep *catalog.Endpoint) (Provider, error) {
			return &fakeProvider{name: "anthropic"}, nil
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}
	if got := len(router.byModel); got != 14 {
		t.Fatalf("len(byModel) = %d, want 14", got)
	}
}

func TestRouter_UnknownModel(t *testing.T) {
	router, err := NewRouter(stubCatalog(), map[string]AdapterFactory{
		"openai": func(ep *catalog.Endpoint) (Provider, error) {
			return &fakeProvider{name: "openai"}, nil
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}

	_, err = router.Complete(context.Background(), &Request{
		Model:    "nonexistent-model-123",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var perr *Error
	if !errors.As(err, &perr) {
		t.Fatalf("err type = %T, want *Error", err)
	}
	if perr.Kind != KindBadRequest {
		t.Fatalf("Kind = %q, want %q", perr.Kind, KindBadRequest)
	}
}

func TestRouter_Dispatch(t *testing.T) {
	openAI := &fakeProvider{name: "openai"}
	anthropic := &fakeProvider{name: "anthropic"}
	router, err := NewRouter(stubCatalog(), map[string]AdapterFactory{
		"openai": func(ep *catalog.Endpoint) (Provider, error) {
			return openAI, nil
		},
		"anthropic": func(ep *catalog.Endpoint) (Provider, error) {
			return anthropic, nil
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}

	req := &Request{Model: "kimi-k2.6", Messages: []Message{{Role: "user", Content: "hi"}}}
	if _, err := router.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if openAI.completeCalls != 1 || openAI.lastCompleteReq.Model != "kimi-k2.6" {
		t.Fatalf("openai Complete calls = %d, model = %q, want 1 / kimi-k2.6", openAI.completeCalls, openAI.lastCompleteReq.Model)
	}
	if anthropic.completeCalls != 0 {
		t.Fatalf("anthropic Complete calls = %d, want 0", anthropic.completeCalls)
	}

	streamReq := &Request{Model: "minimax-m2.5", Messages: []Message{{Role: "user", Content: "hi"}}}
	if _, err := router.CompleteStream(context.Background(), streamReq); err != nil {
		t.Fatalf("CompleteStream() error = %v", err)
	}
	if anthropic.streamCalls != 1 || anthropic.lastStreamReq.Model != "minimax-m2.5" {
		t.Fatalf("anthropic stream calls = %d, model = %q, want 1 / minimax-m2.5", anthropic.streamCalls, anthropic.lastStreamReq.Model)
	}
}

func TestRouter_AliasFallback_PrimarySucceeds(t *testing.T) {
	openAI := &fakeProvider{name: "openai"}
	router := mustRouter(t, fallbackCatalog(), openAI, nil)

	ctx := WithAttemptHolder(context.Background())
	resp, err := router.Complete(ctx, &Request{Model: "coder", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// resp.Model is whatever fakeProvider echoed (req.Model = first chain entry)
	if resp.Model != "deepseek-v4-pro" {
		t.Errorf("resp.Model = %q, want deepseek-v4-pro", resp.Model)
	}
	if openAI.completeCalls != 1 {
		t.Errorf("completeCalls = %d, want 1 (no fallback needed)", openAI.completeCalls)
	}
	atts := AttemptsFromContext(ctx)
	if len(atts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(atts))
	}
	if atts[0].Model != "deepseek-v4-pro" || atts[0].Vendor != "openai" {
		t.Errorf("attempt[0] = %+v, want deepseek-v4-pro / openai", atts[0])
	}
}

func TestRouter_AliasFallback_RetriesOnEligibleError(t *testing.T) {
	// Primary fails with KindRateLimit (eligible) → next chain entry tried.
	openAI := &fakeProvider{name: "openai"}
	openAI.errors = map[string]*Error{
		"deepseek-v4-pro": {Kind: KindRateLimit, Message: "throttled", StatusCode: 429},
	}
	router := mustRouter(t, fallbackCatalog(), openAI, nil)

	ctx := WithAttemptHolder(context.Background())
	resp, err := router.Complete(ctx, &Request{Model: "coder", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Model != "deepseek-v4-flash" {
		t.Errorf("resp.Model = %q, want deepseek-v4-flash (after fallback)", resp.Model)
	}
	atts := AttemptsFromContext(ctx)
	if len(atts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(atts))
	}
	if atts[0].ErrorKind != KindRateLimit || atts[0].StatusCode != 429 {
		t.Errorf("attempt[0] = %+v, want rate_limit/429", atts[0])
	}
	if atts[1].ErrorKind != "" || atts[1].StatusCode != 200 {
		t.Errorf("attempt[1] = %+v, want success", atts[1])
	}
}

func TestRouter_AliasFallback_BadRequestStopsImmediately(t *testing.T) {
	// Primary fails with KindBadRequest (not eligible) → return immediately.
	openAI := &fakeProvider{name: "openai"}
	openAI.errors = map[string]*Error{
		"deepseek-v4-pro": {Kind: KindBadRequest, Message: "malformed"},
	}
	router := mustRouter(t, fallbackCatalog(), openAI, nil)

	ctx := WithAttemptHolder(context.Background())
	_, err := router.Complete(ctx, &Request{Model: "coder", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("Complete: want error")
	}
	var perr *Error
	if !errors.As(err, &perr) || perr.Kind != KindBadRequest {
		t.Fatalf("err = %v, want KindBadRequest", err)
	}
	if openAI.completeCalls != 1 {
		t.Errorf("completeCalls = %d, want 1 (no fallback for non-eligible)", openAI.completeCalls)
	}
	atts := AttemptsFromContext(ctx)
	if len(atts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(atts))
	}
}

func TestRouter_AliasFallback_AllExhausted(t *testing.T) {
	openAI := &fakeProvider{name: "openai"}
	openAI.errorAll = &Error{Kind: KindUpstream, Message: "boom", StatusCode: 502}
	router := mustRouter(t, fallbackCatalog(), openAI, nil)

	ctx := WithAttemptHolder(context.Background())
	_, err := router.Complete(ctx, &Request{Model: "coder", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("Complete: want error")
	}
	var perr *Error
	if !errors.As(err, &perr) || perr.Kind != KindUpstream {
		t.Fatalf("err = %v, want KindUpstream (last attempt err)", err)
	}
	atts := AttemptsFromContext(ctx)
	// chain has 4 openai-protocol entries; all should be tried before chain exhausted.
	if len(atts) != 4 {
		t.Fatalf("attempts = %d, want 4", len(atts))
	}
}

func TestRouter_CircuitOpensAfterRepeatedFailures(t *testing.T) {
	// Only the primary fails — secondary always succeeds. Three failed
	// runs trip the breaker on the primary; the fourth call must skip
	// the primary and hit secondary directly.
	openAI := &fakeProvider{name: "openai"}
	openAI.errors = map[string]*Error{
		"deepseek-v4-pro": {Kind: KindUpstream, Message: "boom"},
	}
	router := mustRouter(t, fallbackCatalog(), openAI, nil)

	for i := 0; i < 3; i++ {
		_, err := router.Complete(context.Background(), &Request{Model: "coder", Messages: []Message{{Role: "user", Content: "x"}}})
		if err != nil {
			t.Fatalf("run %d: unexpected error %v", i, err)
		}
	}
	// 3 runs × 2 calls (pro fail + flash success) = 6 calls.
	if openAI.completeCalls != 6 {
		t.Fatalf("after 3 runs completeCalls = %d, want 6", openAI.completeCalls)
	}

	// Fourth run: primary breaker is open → only flash is called (1 call).
	beforeSkip := openAI.completeCalls
	_, _ = router.Complete(context.Background(), &Request{Model: "coder", Messages: []Message{{Role: "user", Content: "x"}}})
	added := openAI.completeCalls - beforeSkip
	if added != 1 {
		t.Errorf("fourth run added %d calls, want 1 (primary skipped)", added)
	}
}

func TestRouter_RawModelStillWorks(t *testing.T) {
	openAI := &fakeProvider{name: "openai"}
	router := mustRouter(t, fallbackCatalog(), openAI, nil)

	resp, err := router.Complete(context.Background(), &Request{Model: "kimi-k2.6", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Model != "kimi-k2.6" {
		t.Errorf("resp.Model = %q, want kimi-k2.6", resp.Model)
	}
}

func mustRouter(t *testing.T, cat *catalog.Catalog, openAI Provider, anth Provider) *Router {
	t.Helper()
	factories := map[string]AdapterFactory{
		"openai": func(*catalog.Endpoint) (Provider, error) { return openAI, nil },
	}
	if anth != nil {
		factories["anthropic"] = func(*catalog.Endpoint) (Provider, error) { return anth, nil }
	}
	r, err := NewRouter(cat, factories, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return r
}

func fallbackCatalog() *catalog.Catalog {
	cat := stubCatalog()
	cat.Aliases = map[string]*catalog.Alias{
		"coder": {Name: "coder", Chain: []string{"deepseek-v4-pro", "deepseek-v4-flash", "kimi-k2.6", "glm-5.1"}},
	}
	cat.Fallback = catalog.FallbackPolicy{
		OnKinds:         []string{"rate_limit", "upstream", "timeout", "network"},
		CircuitFailures: 3,
		CircuitOpen:     30 * time.Second,
	}
	return cat
}

func stubCatalog() *catalog.Catalog {
	endpoints := map[string]*catalog.Endpoint{
		"opencode-openai": {
			Name:       "opencode-openai",
			Vendor:     "opencode",
			BaseURL:    "http://example.test/openai",
			APIKey:     "key",
			Protocol:   "openai",
			AuthScheme: "bearer",
		},
		"opencode-anthropic": {
			Name:       "opencode-anthropic",
			Vendor:     "opencode",
			BaseURL:    "http://example.test/anthropic",
			APIKey:     "key",
			Protocol:   "anthropic",
			AuthScheme: "x-api-key",
		},
	}
	models := make(map[string]*catalog.Model)
	for _, id := range []string{
		"glm-5.1",
		"glm-5",
		"kimi-k2.5",
		"kimi-k2.6",
		"deepseek-v4-pro",
		"deepseek-v4-flash",
		"mimo-v2-pro",
		"mimo-v2-omni",
		"mimo-v2.5-pro",
		"mimo-v2.5",
		"qwen3.6-plus",
		"qwen3.5-plus",
	} {
		models[id] = &catalog.Model{ID: id, Endpoint: "opencode-openai"}
	}
	for _, id := range []string{"minimax-m2.7", "minimax-m2.5"} {
		models[id] = &catalog.Model{ID: id, Endpoint: "opencode-anthropic"}
	}
	return &catalog.Catalog{
		Endpoints: endpoints,
		Models:    models,
		Defaults:  catalog.Defaults{Model: "deepseek-v4-flash"},
	}
}

type fakeProvider struct {
	name            string
	completeCalls   int
	streamCalls     int
	lastCompleteReq *Request
	lastStreamReq   *Request

	// per-model and global error simulation. Per-model takes precedence.
	errors   map[string]*Error
	errorAll *Error
}

func (p *fakeProvider) Name() string { return p.name }

func (p *fakeProvider) Complete(ctx context.Context, req *Request) (*Response, error) {
	p.completeCalls++
	p.lastCompleteReq = req
	if p.errors != nil {
		if e, ok := p.errors[req.Model]; ok {
			return nil, e
		}
	}
	if p.errorAll != nil {
		return nil, p.errorAll
	}
	return &Response{Model: req.Model, Choices: []Choice{{Index: 0, Message: Message{Role: "assistant", Content: "ok"}}}}, nil
}

func (p *fakeProvider) CompleteStream(ctx context.Context, req *Request) (Stream, error) {
	p.streamCalls++
	p.lastStreamReq = req
	return fakeStream{}, nil
}

type fakeStream struct{}

func (fakeStream) Recv() (*Event, error) { return nil, ErrStreamDone }
func (fakeStream) Close() error          { return nil }
func (fakeStream) Summary() *Summary     { return &Summary{} }
