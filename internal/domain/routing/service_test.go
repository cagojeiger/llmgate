package routing

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"llmgate/internal/catalog"
	"llmgate/internal/llmtypes"
	"llmgate/internal/providers/fake"
)

// testPolicy mirrors the production defaults so the Service behaves the
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

func TestService_EmptyModelsFailsFast(t *testing.T) {
	_, err := NewService(Models{}, Aliases{}, testPolicy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("NewService() error = nil, want empty-models error")
	}
	if !strings.Contains(err.Error(), "no models registered") {
		t.Fatalf("NewService() error = %q, want no-models-registered error", err.Error())
	}
}

func TestService_NilProviderFailsFast(t *testing.T) {
	_, err := NewService(Models{"a": nil}, Aliases{}, testPolicy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("NewService() error = nil, want nil-provider error")
	}
	if !strings.Contains(err.Error(), "nil provider") {
		t.Fatalf("NewService() error = %q, want nil-provider error", err.Error())
	}
}

func TestService_Both(t *testing.T) {
	cat := stubCatalog(t)
	models := buildTestModels(t, cat, fake.NewProvider("openai"), fake.NewProvider("anthropic"))
	aliases := buildTestAliases(cat)
	svc, err := NewService(models, aliases, testPolicy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	if got := len(svc.byModel); got != 14 {
		t.Fatalf("len(byModel) = %d, want 14", got)
	}
}

func TestService_UnknownModel(t *testing.T) {
	cat := stubCatalog(t)
	models := buildTestModels(t, cat, fake.NewProvider("openai"), fake.NewProvider("anthropic"))
	aliases := buildTestAliases(cat)
	svc, err := NewService(models, aliases, testPolicy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}

	_, err = svc.Complete(context.Background(), &llmtypes.Request{
		Model:    "nonexistent-model-123",
		Messages: []llmtypes.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var perr *llmtypes.Error
	if !errors.As(err, &perr) {
		t.Fatalf("err type = %T, want *Error", err)
	}
	if perr.Kind != llmtypes.KindBadRequest {
		t.Fatalf("Kind = %q, want %q", perr.Kind, llmtypes.KindBadRequest)
	}
}

func TestService_Dispatch(t *testing.T) {
	openAI := fake.NewProvider("openai")
	anthropic := fake.NewProvider("anthropic")
	cat := stubCatalog(t)
	models := buildTestModels(t, cat, openAI, anthropic)
	aliases := buildTestAliases(cat)
	svc, err := NewService(models, aliases, testPolicy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}

	req := chatRequest("kimi-k2.6", "hi")
	if _, err := svc.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if openAI.CompleteCalls() != 1 || openAI.LastCompleteRequest().Model != "kimi-k2.6" {
		t.Fatalf(
			"openai Complete calls = %d, model = %q, want 1 / kimi-k2.6",
			openAI.CompleteCalls(),
			openAI.LastCompleteRequest().Model,
		)
	}
	if anthropic.CompleteCalls() != 0 {
		t.Fatalf("anthropic Complete calls = %d, want 0", anthropic.CompleteCalls())
	}

	streamReq := &llmtypes.Request{Model: "minimax-m2.5", Messages: []llmtypes.Message{{Role: "user", Content: "hi"}}}
	streamRes, err := svc.CompleteStream(context.Background(), streamReq)
	if err != nil {
		t.Fatalf("CompleteStream() error = %v", err)
	}
	if streamRes.Stream == nil {
		t.Fatalf("CompleteStream: result.Stream is nil")
	}
	if len(streamRes.Attempts) != 1 || streamRes.Attempts[0].Model != "minimax-m2.5" {
		t.Fatalf("stream attempts = %+v, want one minimax-m2.5", streamRes.Attempts)
	}
	if anthropic.StreamCalls() != 1 || anthropic.LastStreamRequest().Model != "minimax-m2.5" {
		t.Fatalf(
			"anthropic stream calls = %d, model = %q, want 1 / minimax-m2.5",
			anthropic.StreamCalls(),
			anthropic.LastStreamRequest().Model,
		)
	}
}

func TestService_RawModelStillWorks(t *testing.T) {
	openAI := fake.NewProvider("openai")
	svc := mustService(t, fallbackCatalog(t), openAI, nil)

	result, err := svc.Complete(context.Background(), chatRequest("kimi-k2.6", "hi"))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if result.Response == nil || result.Response.Model != "kimi-k2.6" {
		t.Errorf("result.Response.Model = %v, want kimi-k2.6", result.Response)
	}
}

func mustService(t *testing.T, cat *catalog.Catalog, openAI llmtypes.Provider, anth llmtypes.Provider) *Service {
	t.Helper()
	return mustServiceWithPolicy(t, cat, openAI, anth, testPolicy)
}

func mustServiceWithPolicy(
	t *testing.T,
	cat *catalog.Catalog,
	openAI llmtypes.Provider,
	anth llmtypes.Provider,
	policy FallbackPolicy,
) *Service {
	t.Helper()
	models := buildTestModels(t, cat, openAI, anth)
	aliases := buildTestAliases(cat)
	r, err := NewService(models, aliases, policy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return r
}

func chatRequest(model, content string) *llmtypes.Request {
	return &llmtypes.Request{
		Model:    model,
		Messages: []llmtypes.Message{{Role: "user", Content: content}},
	}
}

// buildTestModels turns a catalog into the routing.Models map by
// picking the openai or anthropic provider per the catalog model's
// Protocol field. Tests use this in place of the production
// gateway.BuildRouterInputs. nil anth gets a
// stub fake provider so tests that only care about the openai side
// don't have to construct one.
func buildTestModels(t *testing.T, cat *catalog.Catalog, openAI, anth llmtypes.Provider) Models {
	t.Helper()
	if anth == nil {
		anth = fake.NewProvider("anthropic")
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
// chain-only Aliases shape svc consumes.
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

// stubCatalog builds a Catalog directly (no filesystem round-trip) so svc
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
