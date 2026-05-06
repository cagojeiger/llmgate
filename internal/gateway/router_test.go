package gateway

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
	"llmgate/internal/core"
	"llmgate/internal/streaming"
)

// testPolicy mirrors the production defaults so the router behaves the
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

func TestRouter_EmptyModelsFailsFast(t *testing.T) {
	_, err := NewRouter(Models{}, Aliases{}, testPolicy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("NewRouter() error = nil, want empty-models error")
	}
	if !strings.Contains(err.Error(), "no models registered") {
		t.Fatalf("NewRouter() error = %q, want no-models-registered error", err.Error())
	}
}

func TestRouter_NilProviderFailsFast(t *testing.T) {
	_, err := NewRouter(Models{"a": nil}, Aliases{}, testPolicy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("NewRouter() error = nil, want nil-provider error")
	}
	if !strings.Contains(err.Error(), "nil provider") {
		t.Fatalf("NewRouter() error = %q, want nil-provider error", err.Error())
	}
}

func TestRouter_Both(t *testing.T) {
	cat := stubCatalog(t)
	models := buildTestModels(t, cat, &fakeProvider{name: "openai"}, &fakeProvider{name: "anthropic"})
	aliases := buildTestAliases(cat)
	router, err := NewRouter(models, aliases, testPolicy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}
	if got := len(router.byModel); got != 14 {
		t.Fatalf("len(byModel) = %d, want 14", got)
	}
}

func TestRouter_UnknownModel(t *testing.T) {
	cat := stubCatalog(t)
	models := buildTestModels(t, cat, &fakeProvider{name: "openai"}, &fakeProvider{name: "anthropic"})
	aliases := buildTestAliases(cat)
	router, err := NewRouter(models, aliases, testPolicy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}

	_, err = router.Complete(context.Background(), &core.Request{
		Model:    "nonexistent-model-123",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var perr *core.Error
	if !errors.As(err, &perr) {
		t.Fatalf("err type = %T, want *Error", err)
	}
	if perr.ErrorKind != core.KindBadRequest {
		t.Fatalf("ErrorKind = %q, want %q", perr.ErrorKind, core.KindBadRequest)
	}
}

func TestRouter_Dispatch(t *testing.T) {
	openAI := &fakeProvider{name: "openai"}
	anthropic := &fakeProvider{name: "anthropic"}
	cat := stubCatalog(t)
	models := buildTestModels(t, cat, openAI, anthropic)
	aliases := buildTestAliases(cat)
	router, err := NewRouter(models, aliases, testPolicy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}

	req := &core.Request{Model: "kimi-k2.6", Messages: []core.Message{{Role: "user", Content: "hi"}}}
	if _, err := router.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if openAI.completeCalls != 1 || openAI.lastCompleteReq.Model != "kimi-k2.6" {
		t.Fatalf("openai Complete calls = %d, model = %q, want 1 / kimi-k2.6", openAI.completeCalls, openAI.lastCompleteReq.Model)
	}
	if anthropic.completeCalls != 0 {
		t.Fatalf("anthropic Complete calls = %d, want 0", anthropic.completeCalls)
	}

	streamReq := &core.Request{Model: "minimax-m2.5", Messages: []core.Message{{Role: "user", Content: "hi"}}}
	streamRes, err := router.CompleteStream(context.Background(), streamReq)
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

func TestRouter_AliasFallback_PrimarySucceeds(t *testing.T) {
	openAI := &fakeProvider{name: "openai"}
	router := mustRouter(t, fallbackCatalog(t), openAI, nil)

	result, err := router.Complete(context.Background(), &core.Request{Model: "coder", Messages: []core.Message{{Role: "user", Content: "hi"}}})
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

func TestRouter_AliasFallback_RetriesOnEligibleError(t *testing.T) {
	// Primary fails with KindRateLimit (eligible) → next chain entry tried.
	openAI := &fakeProvider{name: "openai"}
	openAI.errors = map[string]*core.Error{
		"deepseek-v4-pro": {ErrorKind: core.KindRateLimit, Message: "throttled", StatusCode: 429},
	}
	router := mustRouter(t, fallbackCatalog(t), openAI, nil)

	result, err := router.Complete(context.Background(), &core.Request{Model: "coder", Messages: []core.Message{{Role: "user", Content: "hi"}}})
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
	if result.Attempts[0].ErrorKind != core.KindRateLimit || result.Attempts[0].StatusCode != 429 {
		t.Errorf("attempt[0] = %+v, want rate_limit/429", result.Attempts[0])
	}
	if result.Attempts[1].ErrorKind != "" || result.Attempts[1].StatusCode != 200 {
		t.Errorf("attempt[1] = %+v, want success", result.Attempts[1])
	}
}

func TestRouter_StreamAliasFallback_RetriesOnEligiblePreStreamError(t *testing.T) {
	openAI := &fakeProvider{name: "openai"}
	openAI.streamErrors = map[string]*core.Error{
		"deepseek-v4-pro": {ErrorKind: core.KindRateLimit, Message: "stream throttled", StatusCode: 429},
	}
	router := mustRouter(t, fallbackCatalog(t), openAI, nil)

	result, err := router.CompleteStream(context.Background(), &core.Request{Model: "coder", Messages: []core.Message{{Role: "user", Content: "hi"}}})
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
	if result.Attempts[0].Model != "deepseek-v4-pro" || result.Attempts[0].ErrorKind != core.KindRateLimit {
		t.Errorf("attempt[0] = %+v, want deepseek-v4-pro rate_limit", result.Attempts[0])
	}
	if result.Attempts[1].Model != "deepseek-v4-flash" || result.Attempts[1].ErrorKind != "" {
		t.Errorf("attempt[1] = %+v, want deepseek-v4-flash success", result.Attempts[1])
	}
}

func TestRouter_AliasFallback_BadRequestStopsImmediately(t *testing.T) {
	// Primary fails with KindBadRequest (not eligible) → return immediately.
	openAI := &fakeProvider{name: "openai"}
	openAI.errors = map[string]*core.Error{
		"deepseek-v4-pro": {ErrorKind: core.KindBadRequest, Message: "malformed"},
	}
	router := mustRouter(t, fallbackCatalog(t), openAI, nil)

	result, err := router.Complete(context.Background(), &core.Request{Model: "coder", Messages: []core.Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("Complete: want error")
	}
	var perr *core.Error
	if !errors.As(err, &perr) || perr.ErrorKind != core.KindBadRequest {
		t.Fatalf("err = %v, want KindBadRequest", err)
	}
	if openAI.completeCalls != 1 {
		t.Errorf("completeCalls = %d, want 1 (no fallback for non-eligible)", openAI.completeCalls)
	}
	if len(result.Attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(result.Attempts))
	}
}

func TestRouter_StreamAliasFallback_BadRequestStopsImmediately(t *testing.T) {
	openAI := &fakeProvider{name: "openai"}
	openAI.streamErrors = map[string]*core.Error{
		"deepseek-v4-pro": {ErrorKind: core.KindBadRequest, Message: "malformed stream"},
	}
	router := mustRouter(t, fallbackCatalog(t), openAI, nil)

	result, err := router.CompleteStream(context.Background(), &core.Request{Model: "coder", Messages: []core.Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("CompleteStream: want error")
	}
	var perr *core.Error
	if !errors.As(err, &perr) || perr.ErrorKind != core.KindBadRequest {
		t.Fatalf("err = %v, want KindBadRequest", err)
	}
	if openAI.streamCalls != 1 {
		t.Errorf("streamCalls = %d, want 1 (no fallback for non-eligible)", openAI.streamCalls)
	}
	if len(result.Attempts) != 1 || result.Attempts[0].ErrorKind != core.KindBadRequest {
		t.Fatalf("attempts = %+v, want one bad_request attempt", result.Attempts)
	}
}

func TestRouter_AliasFallback_AllExhausted(t *testing.T) {
	openAI := &fakeProvider{name: "openai"}
	openAI.errorAll = &core.Error{ErrorKind: core.KindUpstream, Message: "boom", StatusCode: 502}
	router := mustRouter(t, fallbackCatalog(t), openAI, nil)

	result, err := router.Complete(context.Background(), &core.Request{Model: "coder", Messages: []core.Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("Complete: want error")
	}
	var perr *core.Error
	if !errors.As(err, &perr) || perr.ErrorKind != core.KindUpstream {
		t.Fatalf("err = %v, want KindUpstream (last attempt err)", err)
	}
	// chain has 4 openai-protocol entries; all should be tried before chain exhausted.
	if len(result.Attempts) != 4 {
		t.Fatalf("attempts = %d, want 4", len(result.Attempts))
	}
}

func TestRouter_StreamSkipsOpenCircuitModel(t *testing.T) {
	openAI := &fakeProvider{name: "openai"}
	openAI.errors = map[string]*core.Error{
		"deepseek-v4-pro": {ErrorKind: core.KindUpstream, Message: "boom"},
	}
	router := mustRouter(t, fallbackCatalog(t), openAI, nil)

	for i := 0; i < 3; i++ {
		_, err := router.Complete(context.Background(), &core.Request{Model: "coder", Messages: []core.Message{{Role: "user", Content: "x"}}})
		if err != nil {
			t.Fatalf("run %d: unexpected error %v", i, err)
		}
	}

	result, err := router.CompleteStream(context.Background(), &core.Request{Model: "coder", Messages: []core.Message{{Role: "user", Content: "x"}}})
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

func TestRouter_StreamPreStreamFailuresOpenCircuit(t *testing.T) {
	openAI := &fakeProvider{name: "openai"}
	openAI.streamErrors = map[string]*core.Error{
		"deepseek-v4-pro": {ErrorKind: core.KindUpstream, Message: "stream setup failed"},
	}
	router := mustRouter(t, fallbackCatalog(t), openAI, nil)

	for i := 0; i < 3; i++ {
		result, err := router.CompleteStream(context.Background(), &core.Request{Model: "coder", Messages: []core.Message{{Role: "user", Content: "x"}}})
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
	result, err := router.CompleteStream(context.Background(), &core.Request{Model: "coder", Messages: []core.Message{{Role: "user", Content: "x"}}})
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

func TestRouter_StreamEmptyFirstEventFallsBack(t *testing.T) {
	openAI := &fakeProvider{
		name: "openai",
		streamEmptyEOF: map[string]bool{
			"deepseek-v4-pro": true,
		},
	}
	router := mustRouter(t, fallbackCatalog(t), openAI, nil)

	result, err := router.CompleteStream(context.Background(), &core.Request{Model: "coder", Messages: []core.Message{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	if result.ModelUsed != "deepseek-v4-flash" {
		t.Fatalf("ModelUsed = %q, want deepseek-v4-flash after primary empty stream", result.ModelUsed)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(result.Attempts))
	}
	if result.Attempts[0].ErrorKind != core.KindUpstream {
		t.Fatalf("attempt[0].ErrorKind = %q, want upstream", result.Attempts[0].ErrorKind)
	}
}

func TestRouter_CircuitOpensAfterRepeatedFailures(t *testing.T) {
	// Only the primary fails — secondary always succeeds. Three failed
	// runs trip the breaker on the primary; the fourth call must skip
	// the primary and hit secondary directly.
	openAI := &fakeProvider{name: "openai"}
	openAI.errors = map[string]*core.Error{
		"deepseek-v4-pro": {ErrorKind: core.KindUpstream, Message: "boom"},
	}
	router := mustRouter(t, fallbackCatalog(t), openAI, nil)

	for i := 0; i < 3; i++ {
		_, err := router.Complete(context.Background(), &core.Request{Model: "coder", Messages: []core.Message{{Role: "user", Content: "x"}}})
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
	_, _ = router.Complete(context.Background(), &core.Request{Model: "coder", Messages: []core.Message{{Role: "user", Content: "x"}}})
	added := openAI.completeCalls - beforeSkip
	if added != 1 {
		t.Errorf("fourth run added %d calls, want 1 (primary skipped)", added)
	}
}

func TestRouter_CompleteTimeoutFallsBack(t *testing.T) {
	openAI := &fakeProvider{
		name: "openai",
		completeDelays: map[string]time.Duration{
			"deepseek-v4-pro": 50 * time.Millisecond,
		},
	}
	policy := testPolicy
	policy.CompleteTimeout = time.Millisecond
	router := mustRouterWithPolicy(t, fallbackCatalog(t), openAI, nil, policy)

	result, err := router.Complete(context.Background(), &core.Request{Model: "coder", Messages: []core.Message{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if result.ModelUsed != "deepseek-v4-flash" {
		t.Fatalf("ModelUsed = %q, want deepseek-v4-flash after primary timeout", result.ModelUsed)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(result.Attempts))
	}
	if result.Attempts[0].ErrorKind != core.KindTimeout {
		t.Fatalf("attempt[0].ErrorKind = %q, want timeout", result.Attempts[0].ErrorKind)
	}
}

func TestRouter_RequestTimeoutStopsChain(t *testing.T) {
	openAI := &fakeProvider{
		name: "openai",
		completeDelays: map[string]time.Duration{
			"deepseek-v4-pro": 50 * time.Millisecond,
		},
	}
	policy := testPolicy
	policy.CompleteTimeout = time.Minute
	router := mustRouterWithPolicy(t, fallbackCatalog(t), openAI, nil, policy)

	// Request-level deadline lives on the caller's ctx (handler does this in
	// production); router itself no longer adds a routeCtx wrap.
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	result, err := router.Complete(ctx, &core.Request{Model: "coder", Messages: []core.Message{{Role: "user", Content: "x"}}})
	if err == nil {
		t.Fatal("Complete: want request timeout error")
	}
	var perr *core.Error
	if !errors.As(err, &perr) || perr.ErrorKind != core.KindTimeout {
		t.Fatalf("err = %v, want provider timeout", err)
	}
	if openAI.completeCalls != 1 {
		t.Fatalf("completeCalls = %d, want 1 (request budget exhausted before fallback)", openAI.completeCalls)
	}
	if len(result.Attempts) != 1 || result.Attempts[0].ErrorKind != core.KindTimeout {
		t.Fatalf("attempts = %+v, want one timeout attempt", result.Attempts)
	}
}

func TestRouter_RawModelStillWorks(t *testing.T) {
	openAI := &fakeProvider{name: "openai"}
	router := mustRouter(t, fallbackCatalog(t), openAI, nil)

	result, err := router.Complete(context.Background(), &core.Request{Model: "kimi-k2.6", Messages: []core.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if result.Response == nil || result.Response.Model != "kimi-k2.6" {
		t.Errorf("result.Response.Model = %v, want kimi-k2.6", result.Response)
	}
}

func mustRouter(t *testing.T, cat *catalog.Catalog, openAI core.Provider, anth core.Provider) *Router {
	t.Helper()
	return mustRouterWithPolicy(t, cat, openAI, anth, testPolicy)
}

func mustRouterWithPolicy(t *testing.T, cat *catalog.Catalog, openAI core.Provider, anth core.Provider, policy FallbackPolicy) *Router {
	t.Helper()
	models := buildTestModels(t, cat, openAI, anth)
	aliases := buildTestAliases(cat)
	r, err := NewRouter(models, aliases, policy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return r
}

// buildTestModels turns a catalog into the gateway.Models map by
// picking the openai or anthropic provider per the catalog model's
// Protocol field. Tests use this in place of the production
// buildRouterInputs (which lives in cmd/llmgate). nil anth gets a
// stub fakeProvider so tests that only care about the openai side
// don't have to construct one.
func buildTestModels(t *testing.T, cat *catalog.Catalog, openAI, anth core.Provider) Models {
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
// chain-only Aliases shape router consumes.
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

// stubCatalog builds a Catalog directly (no filesystem round-trip) so router
// tests stay focused on routing / fallback / breaker behavior. The real
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
	lastCompleteReq *core.Request
	lastStreamReq   *core.Request

	// per-model and global error simulation. Per-model takes precedence.
	errors           map[string]*core.Error
	errorAll         *core.Error
	streamErrors     map[string]*core.Error
	streamErrorAll   *core.Error
	completeDelays   map[string]time.Duration
	streamDelays     map[string]time.Duration
	streamRecvDelays map[string]time.Duration
	streamEmptyEOF   map[string]bool
}

func (p *fakeProvider) Name() string { return p.name }

func (p *fakeProvider) Complete(ctx context.Context, req *core.Request) (*core.Response, error) {
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
	return &core.Response{Model: req.Model, Choices: []core.Choice{{Index: 0, Message: core.Message{Role: "assistant", Content: "ok"}}}}, nil
}

func (p *fakeProvider) CompleteStream(ctx context.Context, req *core.Request) (core.Stream, error) {
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
	events := []*core.Event{
		{Choices: []core.ChoiceDelta{{Delta: core.Delta{Content: "ok"}}}},
	}
	if p.streamEmptyEOF[req.Model] {
		events = nil
	}
	raw := &fakeStream{
		events:    events,
		recvDelay: p.streamRecvDelays[req.Model],
	}
	return streaming.ValidateStreamStart(ctx, raw)
}

type fakeStream struct {
	mu        sync.Mutex
	closeOnce sync.Once
	done      chan struct{}
	events    []*core.Event
	cursor    int
	recvDelay time.Duration
}

func (s *fakeStream) Recv() (*core.Event, error) {
	if s.recvDelay > 0 {
		select {
		case <-time.After(s.recvDelay):
		case <-s.doneChan():
			return nil, core.ErrStreamDone
		}
	}
	if s.cursor < len(s.events) {
		event := s.events[s.cursor]
		s.cursor++
		return event, nil
	}
	return nil, core.ErrStreamDone
}

func (s *fakeStream) Close() error {
	done := s.doneChan()
	s.closeOnce.Do(func() { close(done) })
	return nil
}

func (s *fakeStream) Summary() *core.Summary { return &core.Summary{} }

func (s *fakeStream) doneChan() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done == nil {
		s.done = make(chan struct{})
	}
	return s.done
}
