package dispatch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"llmgate/internal/catalog"
	"llmgate/internal/provider"
)

// testPolicy mirrors the production defaults so the dispatcher behaves the
// same in tests as it does at runtime — fallback on transient classes,
// circuit trips after 3 strikes, 30s cooldown.
var testPolicy = FallbackPolicy{
	OnKinds:         []string{"rate_limit", "upstream", "timeout", "network"},
	CircuitFailures: 3,
	CircuitOpen:     30 * time.Second,
	CircuitMaxOpen:  5 * time.Minute,
	CircuitJitter:   0.2,
	CompleteTimeout: time.Minute,
}

func TestDispatcher_EmptyModelsFailsFast(t *testing.T) {
	_, err := NewDispatcher(Models{}, Aliases{}, testPolicy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("NewDispatcher() error = nil, want empty-models error")
	}
	if !strings.Contains(err.Error(), "no models registered") {
		t.Fatalf("NewDispatcher() error = %q, want no-models-registered error", err.Error())
	}
}

func TestDispatcher_NilProviderFailsFast(t *testing.T) {
	_, err := NewDispatcher(Models{"a": nil}, Aliases{}, testPolicy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("NewDispatcher() error = nil, want nil-provider error")
	}
	if !strings.Contains(err.Error(), "nil provider") {
		t.Fatalf("NewDispatcher() error = %q, want nil-provider error", err.Error())
	}
}

func TestDispatcher_Both(t *testing.T) {
	cat := stubCatalog(t)
	models := buildTestModels(t, cat, &fakeProvider{name: "openai"}, &fakeProvider{name: "anthropic"})
	aliases := buildTestAliases(cat)
	dispatcher, err := NewDispatcher(models, aliases, testPolicy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewDispatcher() error = %v", err)
	}
	if got := len(dispatcher.byModel); got != 14 {
		t.Fatalf("len(byModel) = %d, want 14", got)
	}
}

func TestDispatcher_UnknownModel(t *testing.T) {
	cat := stubCatalog(t)
	models := buildTestModels(t, cat, &fakeProvider{name: "openai"}, &fakeProvider{name: "anthropic"})
	aliases := buildTestAliases(cat)
	dispatcher, err := NewDispatcher(models, aliases, testPolicy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewDispatcher() error = %v", err)
	}

	_, err = dispatcher.Complete(context.Background(), &provider.Request{
		Model:    "nonexistent-model-123",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var perr *provider.Error
	if !errors.As(err, &perr) {
		t.Fatalf("err type = %T, want *Error", err)
	}
	if perr.Kind != provider.KindBadRequest {
		t.Fatalf("Kind = %q, want %q", perr.Kind, provider.KindBadRequest)
	}
}

func TestDispatcher_Dispatch(t *testing.T) {
	openAI := &fakeProvider{name: "openai"}
	anthropic := &fakeProvider{name: "anthropic"}
	cat := stubCatalog(t)
	models := buildTestModels(t, cat, openAI, anthropic)
	aliases := buildTestAliases(cat)
	dispatcher, err := NewDispatcher(models, aliases, testPolicy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewDispatcher() error = %v", err)
	}

	req := &provider.Request{Model: "kimi-k2.6", Messages: []provider.Message{{Role: "user", Content: "hi"}}}
	if _, err := dispatcher.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if openAI.completeCalls != 1 || openAI.lastCompleteReq.Model != "kimi-k2.6" {
		t.Fatalf("openai Complete calls = %d, model = %q, want 1 / kimi-k2.6", openAI.completeCalls, openAI.lastCompleteReq.Model)
	}
	if anthropic.completeCalls != 0 {
		t.Fatalf("anthropic Complete calls = %d, want 0", anthropic.completeCalls)
	}

	streamReq := &provider.Request{Model: "minimax-m2.5", Messages: []provider.Message{{Role: "user", Content: "hi"}}}
	streamRes, err := dispatcher.CompleteStream(context.Background(), streamReq)
	if err != nil {
		t.Fatalf("CompleteStream() error = %v", err)
	}
	if streamRes.Stream == nil {
		t.Fatalf("CompleteStream: result.Stream is nil")
	}
	if len(streamRes.Attempts) != 1 || streamRes.Attempts[0].Model != "minimax-m2.5" {
		t.Fatalf("stream attempts = %+v, want one minimax-m2.5", streamRes.Attempts)
	}
	if anthropic.streamCalls != 1 || anthropic.lastStreamReq.Model != "minimax-m2.5" {
		t.Fatalf("anthropic stream calls = %d, model = %q, want 1 / minimax-m2.5", anthropic.streamCalls, anthropic.lastStreamReq.Model)
	}
}

func TestDispatcher_AliasFallback_PrimarySucceeds(t *testing.T) {
	openAI := &fakeProvider{name: "openai"}
	dispatcher := mustDispatcher(t, fallbackCatalog(t), openAI, nil)

	result, err := dispatcher.Complete(context.Background(), &provider.Request{Model: "coder", Messages: []provider.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if result.Response == nil || result.Response.Model != "deepseek-v4-pro" {
		t.Errorf("result.Response.Model = %v, want deepseek-v4-pro", result.Response)
	}
	if result.Vendor != "openai" || result.ModelUsed != "deepseek-v4-pro" {
		t.Errorf("Vendor/ModelUsed = %q/%q, want openai/deepseek-v4-pro", result.Vendor, result.ModelUsed)
	}
	if openAI.completeCalls != 1 {
		t.Errorf("completeCalls = %d, want 1 (no fallback needed)", openAI.completeCalls)
	}
	if len(result.Attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(result.Attempts))
	}
	if result.Attempts[0].Model != "deepseek-v4-pro" || result.Attempts[0].Vendor != "openai" {
		t.Errorf("attempt[0] = %+v, want deepseek-v4-pro / openai", result.Attempts[0])
	}
}

func TestDispatcher_AliasFallback_RetriesOnEligibleError(t *testing.T) {
	// Primary fails with KindRateLimit (eligible) → next chain entry tried.
	openAI := &fakeProvider{name: "openai"}
	openAI.errors = map[string]*provider.Error{
		"deepseek-v4-pro": {Kind: provider.KindRateLimit, Message: "throttled", StatusCode: 429},
	}
	dispatcher := mustDispatcher(t, fallbackCatalog(t), openAI, nil)

	result, err := dispatcher.Complete(context.Background(), &provider.Request{Model: "coder", Messages: []provider.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if result.Response == nil || result.Response.Model != "deepseek-v4-flash" {
		t.Errorf("result.Response.Model = %v, want deepseek-v4-flash (after fallback)", result.Response)
	}
	if result.ModelUsed != "deepseek-v4-flash" {
		t.Errorf("ModelUsed = %q, want deepseek-v4-flash", result.ModelUsed)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(result.Attempts))
	}
	if result.Attempts[0].ErrorKind != provider.KindRateLimit || result.Attempts[0].StatusCode != 429 {
		t.Errorf("attempt[0] = %+v, want rate_limit/429", result.Attempts[0])
	}
	if result.Attempts[1].ErrorKind != "" || result.Attempts[1].StatusCode != 200 {
		t.Errorf("attempt[1] = %+v, want success", result.Attempts[1])
	}
}

func TestDispatcher_StreamAliasFallback_RetriesOnEligiblePreStreamError(t *testing.T) {
	openAI := &fakeProvider{name: "openai"}
	openAI.streamErrors = map[string]*provider.Error{
		"deepseek-v4-pro": {Kind: provider.KindRateLimit, Message: "stream throttled", StatusCode: 429},
	}
	dispatcher := mustDispatcher(t, fallbackCatalog(t), openAI, nil)

	result, err := dispatcher.CompleteStream(context.Background(), &provider.Request{Model: "coder", Messages: []provider.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	if result.Stream == nil {
		t.Fatal("CompleteStream: result.Stream is nil")
	}
	if result.ModelUsed != "deepseek-v4-flash" {
		t.Errorf("ModelUsed = %q, want deepseek-v4-flash", result.ModelUsed)
	}
	if openAI.streamCalls != 2 || openAI.lastStreamReq.Model != "deepseek-v4-flash" {
		t.Fatalf("stream calls/model = %d/%q, want 2/deepseek-v4-flash", openAI.streamCalls, openAI.lastStreamReq.Model)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(result.Attempts))
	}
	if result.Attempts[0].Model != "deepseek-v4-pro" || result.Attempts[0].ErrorKind != provider.KindRateLimit {
		t.Errorf("attempt[0] = %+v, want deepseek-v4-pro rate_limit", result.Attempts[0])
	}
	if result.Attempts[1].Model != "deepseek-v4-flash" || result.Attempts[1].ErrorKind != "" {
		t.Errorf("attempt[1] = %+v, want deepseek-v4-flash success", result.Attempts[1])
	}
}

func TestDispatcher_AliasFallback_BadRequestStopsImmediately(t *testing.T) {
	// Primary fails with KindBadRequest (not eligible) → return immediately.
	openAI := &fakeProvider{name: "openai"}
	openAI.errors = map[string]*provider.Error{
		"deepseek-v4-pro": {Kind: provider.KindBadRequest, Message: "malformed"},
	}
	dispatcher := mustDispatcher(t, fallbackCatalog(t), openAI, nil)

	result, err := dispatcher.Complete(context.Background(), &provider.Request{Model: "coder", Messages: []provider.Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("Complete: want error")
	}
	var perr *provider.Error
	if !errors.As(err, &perr) || perr.Kind != provider.KindBadRequest {
		t.Fatalf("err = %v, want KindBadRequest", err)
	}
	if openAI.completeCalls != 1 {
		t.Errorf("completeCalls = %d, want 1 (no fallback for non-eligible)", openAI.completeCalls)
	}
	if len(result.Attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(result.Attempts))
	}
}

func TestDispatcher_StreamAliasFallback_BadRequestStopsImmediately(t *testing.T) {
	openAI := &fakeProvider{name: "openai"}
	openAI.streamErrors = map[string]*provider.Error{
		"deepseek-v4-pro": {Kind: provider.KindBadRequest, Message: "malformed stream"},
	}
	dispatcher := mustDispatcher(t, fallbackCatalog(t), openAI, nil)

	result, err := dispatcher.CompleteStream(context.Background(), &provider.Request{Model: "coder", Messages: []provider.Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("CompleteStream: want error")
	}
	var perr *provider.Error
	if !errors.As(err, &perr) || perr.Kind != provider.KindBadRequest {
		t.Fatalf("err = %v, want KindBadRequest", err)
	}
	if openAI.streamCalls != 1 {
		t.Errorf("streamCalls = %d, want 1 (no fallback for non-eligible)", openAI.streamCalls)
	}
	if len(result.Attempts) != 1 || result.Attempts[0].ErrorKind != provider.KindBadRequest {
		t.Fatalf("attempts = %+v, want one bad_request attempt", result.Attempts)
	}
}

func TestDispatcher_AliasFallback_AllExhausted(t *testing.T) {
	openAI := &fakeProvider{name: "openai"}
	openAI.errorAll = &provider.Error{Kind: provider.KindUpstream, Message: "boom", StatusCode: 502}
	dispatcher := mustDispatcher(t, fallbackCatalog(t), openAI, nil)

	result, err := dispatcher.Complete(context.Background(), &provider.Request{Model: "coder", Messages: []provider.Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("Complete: want error")
	}
	var perr *provider.Error
	if !errors.As(err, &perr) || perr.Kind != provider.KindUpstream {
		t.Fatalf("err = %v, want KindUpstream (last attempt err)", err)
	}
	// chain has 4 openai-protocol entries; all should be tried before chain exhausted.
	if len(result.Attempts) != 4 {
		t.Fatalf("attempts = %d, want 4", len(result.Attempts))
	}
}

func TestDispatcher_StreamSkipsOpenCircuitModel(t *testing.T) {
	openAI := &fakeProvider{name: "openai"}
	openAI.errors = map[string]*provider.Error{
		"deepseek-v4-pro": {Kind: provider.KindUpstream, Message: "boom"},
	}
	dispatcher := mustDispatcher(t, fallbackCatalog(t), openAI, nil)

	for i := 0; i < 3; i++ {
		_, err := dispatcher.Complete(context.Background(), &provider.Request{Model: "coder", Messages: []provider.Message{{Role: "user", Content: "x"}}})
		if err != nil {
			t.Fatalf("run %d: unexpected error %v", i, err)
		}
	}

	result, err := dispatcher.CompleteStream(context.Background(), &provider.Request{Model: "coder", Messages: []provider.Message{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	if result.ModelUsed != "deepseek-v4-flash" {
		t.Fatalf("ModelUsed = %q, want deepseek-v4-flash (primary circuit open)", result.ModelUsed)
	}
	if openAI.streamCalls != 1 || openAI.lastStreamReq.Model != "deepseek-v4-flash" {
		t.Fatalf("stream calls/model = %d/%q, want 1/deepseek-v4-flash", openAI.streamCalls, openAI.lastStreamReq.Model)
	}
	if len(result.Attempts) != 1 || result.Attempts[0].Model != "deepseek-v4-flash" {
		t.Fatalf("attempts = %+v, want one flash attempt", result.Attempts)
	}
}

func TestDispatcher_StreamPreStreamFailuresOpenCircuit(t *testing.T) {
	openAI := &fakeProvider{name: "openai"}
	openAI.streamErrors = map[string]*provider.Error{
		"deepseek-v4-pro": {Kind: provider.KindUpstream, Message: "stream setup failed"},
	}
	dispatcher := mustDispatcher(t, fallbackCatalog(t), openAI, nil)

	for i := 0; i < 3; i++ {
		result, err := dispatcher.CompleteStream(context.Background(), &provider.Request{Model: "coder", Messages: []provider.Message{{Role: "user", Content: "x"}}})
		if err != nil {
			t.Fatalf("run %d: unexpected error %v", i, err)
		}
		if result.ModelUsed != "deepseek-v4-flash" {
			t.Fatalf("run %d ModelUsed = %q, want deepseek-v4-flash", i, result.ModelUsed)
		}
	}
	if openAI.streamCalls != 6 {
		t.Fatalf("after 3 runs streamCalls = %d, want 6", openAI.streamCalls)
	}

	beforeSkip := openAI.streamCalls
	result, err := dispatcher.CompleteStream(context.Background(), &provider.Request{Model: "coder", Messages: []provider.Message{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatalf("fourth CompleteStream: %v", err)
	}
	if added := openAI.streamCalls - beforeSkip; added != 1 {
		t.Fatalf("fourth run added %d stream calls, want 1 (primary skipped)", added)
	}
	if result.ModelUsed != "deepseek-v4-flash" {
		t.Fatalf("fourth ModelUsed = %q, want deepseek-v4-flash", result.ModelUsed)
	}
}

func TestDispatcher_StreamEmptyFirstEventFallsBack(t *testing.T) {
	openAI := &fakeProvider{
		name: "openai",
		streamEmptyEOF: map[string]bool{
			"deepseek-v4-pro": true,
		},
	}
	dispatcher := mustDispatcher(t, fallbackCatalog(t), openAI, nil)

	result, err := dispatcher.CompleteStream(context.Background(), &provider.Request{Model: "coder", Messages: []provider.Message{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	if result.ModelUsed != "deepseek-v4-flash" {
		t.Fatalf("ModelUsed = %q, want deepseek-v4-flash after primary empty stream", result.ModelUsed)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(result.Attempts))
	}
	if result.Attempts[0].ErrorKind != provider.KindUpstream {
		t.Fatalf("attempt[0].ErrorKind = %q, want upstream", result.Attempts[0].ErrorKind)
	}
}

func TestDispatcher_CircuitOpensAfterRepeatedFailures(t *testing.T) {
	// Only the primary fails — secondary always succeeds. Three failed
	// runs trip the breaker on the primary; the fourth call must skip
	// the primary and hit secondary directly.
	openAI := &fakeProvider{name: "openai"}
	openAI.errors = map[string]*provider.Error{
		"deepseek-v4-pro": {Kind: provider.KindUpstream, Message: "boom"},
	}
	dispatcher := mustDispatcher(t, fallbackCatalog(t), openAI, nil)

	for i := 0; i < 3; i++ {
		_, err := dispatcher.Complete(context.Background(), &provider.Request{Model: "coder", Messages: []provider.Message{{Role: "user", Content: "x"}}})
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
	_, _ = dispatcher.Complete(context.Background(), &provider.Request{Model: "coder", Messages: []provider.Message{{Role: "user", Content: "x"}}})
	added := openAI.completeCalls - beforeSkip
	if added != 1 {
		t.Errorf("fourth run added %d calls, want 1 (primary skipped)", added)
	}
}

func TestDispatcher_CompleteTimeoutFallsBack(t *testing.T) {
	openAI := &fakeProvider{
		name: "openai",
		completeDelays: map[string]time.Duration{
			"deepseek-v4-pro": 50 * time.Millisecond,
		},
	}
	policy := testPolicy
	policy.CompleteTimeout = time.Millisecond
	dispatcher := mustDispatcherWithPolicy(t, fallbackCatalog(t), openAI, nil, policy)

	result, err := dispatcher.Complete(context.Background(), &provider.Request{Model: "coder", Messages: []provider.Message{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if result.ModelUsed != "deepseek-v4-flash" {
		t.Fatalf("ModelUsed = %q, want deepseek-v4-flash after primary timeout", result.ModelUsed)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(result.Attempts))
	}
	if result.Attempts[0].ErrorKind != provider.KindTimeout {
		t.Fatalf("attempt[0].ErrorKind = %q, want timeout", result.Attempts[0].ErrorKind)
	}
}

func TestDispatcher_RequestTimeoutStopsChain(t *testing.T) {
	openAI := &fakeProvider{
		name: "openai",
		completeDelays: map[string]time.Duration{
			"deepseek-v4-pro": 50 * time.Millisecond,
		},
	}
	policy := testPolicy
	policy.CompleteTimeout = time.Minute
	dispatcher := mustDispatcherWithPolicy(t, fallbackCatalog(t), openAI, nil, policy)

	// Request-level deadline lives on the caller's ctx (handler does this in
	// production); dispatcher itself no longer adds a routeCtx wrap.
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	result, err := dispatcher.Complete(ctx, &provider.Request{Model: "coder", Messages: []provider.Message{{Role: "user", Content: "x"}}})
	if err == nil {
		t.Fatal("Complete: want request timeout error")
	}
	var perr *provider.Error
	if !errors.As(err, &perr) || perr.Kind != provider.KindTimeout {
		t.Fatalf("err = %v, want provider timeout", err)
	}
	if openAI.completeCalls != 1 {
		t.Fatalf("completeCalls = %d, want 1 (request budget exhausted before fallback)", openAI.completeCalls)
	}
	if len(result.Attempts) != 1 || result.Attempts[0].ErrorKind != provider.KindTimeout {
		t.Fatalf("attempts = %+v, want one timeout attempt", result.Attempts)
	}
}

func TestDispatcher_RawModelStillWorks(t *testing.T) {
	openAI := &fakeProvider{name: "openai"}
	dispatcher := mustDispatcher(t, fallbackCatalog(t), openAI, nil)

	result, err := dispatcher.Complete(context.Background(), &provider.Request{Model: "kimi-k2.6", Messages: []provider.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if result.Response == nil || result.Response.Model != "kimi-k2.6" {
		t.Errorf("result.Response.Model = %v, want kimi-k2.6", result.Response)
	}
}

func mustDispatcher(t *testing.T, cat *catalog.Catalog, openAI provider.Provider, anth provider.Provider) *Dispatcher {
	t.Helper()
	return mustDispatcherWithPolicy(t, cat, openAI, anth, testPolicy)
}

func mustDispatcherWithPolicy(t *testing.T, cat *catalog.Catalog, openAI provider.Provider, anth provider.Provider, policy FallbackPolicy) *Dispatcher {
	t.Helper()
	models := buildTestModels(t, cat, openAI, anth)
	aliases := buildTestAliases(cat)
	r, err := NewDispatcher(models, aliases, policy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	return r
}

// buildTestModels turns a catalog into the dispatch.Models map by
// picking the openai or anthropic provider per the catalog model's
// Protocol field. Tests use this in place of the production
// buildDispatcherInputs (which lives in cmd/llmgate). nil anth gets a
// stub fakeProvider so tests that only care about the openai side
// don't have to construct one.
func buildTestModels(t *testing.T, cat *catalog.Catalog, openAI, anth provider.Provider) Models {
	t.Helper()
	if anth == nil {
		anth = &fakeProvider{name: "anthropic"}
	}
	m := make(Models, len(cat.Models))
	for id, mc := range cat.Models {
		switch mc.Protocol {
		case "openai":
			m[id] = openAI
		case "anthropic":
			m[id] = anth
		default:
			t.Fatalf("buildTestModels: unknown protocol %q for model %q", mc.Protocol, id)
		}
	}
	return m
}

// buildTestAliases turns the catalog's alias entries into the simpler
// chain-only Aliases shape dispatcher consumes.
func buildTestAliases(cat *catalog.Catalog) Aliases {
	a := make(Aliases, len(cat.Aliases))
	for name, al := range cat.Aliases {
		a[name] = append([]string(nil), al.Chain...)
	}
	return a
}

// fallbackCatalog mirrors stubCatalog with the addition of a "coder" alias
// whose chain spans the openai-protocol models in priority order. Used by
// every fallback / circuit-breaker test in this file so the chain shape is
// shared across cases.
func fallbackCatalog(t *testing.T) *catalog.Catalog {
	cat := stubCatalog(t)
	cat.Aliases = map[string]*catalog.Alias{
		"coder": {Alias: "coder", Chain: []string{"deepseek-v4-pro", "deepseek-v4-flash", "kimi-k2.6", "glm-5.1"}},
	}
	return cat
}

// stubCatalog builds a Catalog directly (no filesystem round-trip) so dispatcher
// tests stay focused on dispatch / fallback / breaker behavior. The real
// loader is exercised in catalog_test.go.
func stubCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	cat := &catalog.Catalog{
		Models:  make(map[string]*catalog.Model),
		Aliases: make(map[string]*catalog.Alias),
	}
	openaiIDs := []string{
		"glm-5.1", "glm-5",
		"kimi-k2.5", "kimi-k2.6",
		"deepseek-v4-pro", "deepseek-v4-flash",
		"mimo-v2-pro", "mimo-v2-omni", "mimo-v2.5-pro", "mimo-v2.5",
		"qwen3.6-plus", "qwen3.5-plus",
	}
	for _, id := range openaiIDs {
		cat.Models[id] = &catalog.Model{
			ID: id, Vendor: "opencode", Protocol: "openai",
			BaseURL: "http://example.test/v1", AuthEnv: "TEST_API_KEY", AuthScheme: "bearer",
		}
	}
	for _, id := range []string{"minimax-m2.7", "minimax-m2.5"} {
		cat.Models[id] = &catalog.Model{
			ID: id, Vendor: "opencode", Protocol: "anthropic",
			BaseURL: "http://example.test/v1", AuthEnv: "TEST_API_KEY", AuthScheme: "x-api-key",
		}
	}
	return cat
}

type fakeProvider struct {
	name            string
	completeCalls   int
	streamCalls     int
	lastCompleteReq *provider.Request
	lastStreamReq   *provider.Request

	// per-model and global error simulation. Per-model takes precedence.
	errors           map[string]*provider.Error
	errorAll         *provider.Error
	streamErrors     map[string]*provider.Error
	streamErrorAll   *provider.Error
	completeDelays   map[string]time.Duration
	streamDelays     map[string]time.Duration
	streamRecvDelays map[string]time.Duration
	streamEmptyEOF   map[string]bool
}

func (p *fakeProvider) Name() string { return p.name }

func (p *fakeProvider) Complete(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	p.completeCalls++
	p.lastCompleteReq = req
	if p.completeDelays != nil {
		if d := p.completeDelays[req.Model]; d > 0 {
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	if p.errors != nil {
		if e, ok := p.errors[req.Model]; ok {
			return nil, e
		}
	}
	if p.errorAll != nil {
		return nil, p.errorAll
	}
	return &provider.Response{Model: req.Model, Choices: []provider.Choice{{Index: 0, Message: provider.Message{Role: "assistant", Content: "ok"}}}}, nil
}

func (p *fakeProvider) CompleteStream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	p.streamCalls++
	p.lastStreamReq = req
	if p.streamDelays != nil {
		if d := p.streamDelays[req.Model]; d > 0 {
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	if p.streamErrors != nil {
		if e, ok := p.streamErrors[req.Model]; ok {
			return nil, e
		}
	}
	if p.streamErrorAll != nil {
		return nil, p.streamErrorAll
	}
	events := []*provider.Event{
		{Choices: []provider.ChoiceDelta{{Delta: provider.Delta{Content: "ok"}}}},
	}
	if p.streamEmptyEOF[req.Model] {
		events = nil
	}
	raw := &fakeStream{
		events:    events,
		recvDelay: p.streamRecvDelays[req.Model],
	}
	return provider.ValidateFirstEvent(ctx, raw)
}

type fakeStream struct {
	mu        sync.Mutex
	closeOnce sync.Once
	done      chan struct{}
	events    []*provider.Event
	cursor    int
	recvDelay time.Duration
}

func (s *fakeStream) Recv() (*provider.Event, error) {
	if s.recvDelay > 0 {
		select {
		case <-time.After(s.recvDelay):
		case <-s.doneChan():
			return nil, provider.ErrStreamDone
		}
	}
	if s.cursor < len(s.events) {
		event := s.events[s.cursor]
		s.cursor++
		return event, nil
	}
	return nil, provider.ErrStreamDone
}

func (s *fakeStream) Close() error {
	done := s.doneChan()
	s.closeOnce.Do(func() { close(done) })
	return nil
}

func (s *fakeStream) Summary() *provider.Summary { return &provider.Summary{} }

func (s *fakeStream) doneChan() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done == nil {
		s.done = make(chan struct{})
	}
	return s.done
}
