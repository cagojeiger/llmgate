package llmtypes

import (
	"encoding/json"
	"testing"
)

func TestAttachReportedCost(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{name: "number", raw: `0.0012`, want: `0.0012`, ok: true},
		{name: "numeric string", raw: `"0.0012"`, want: `0.0012`, ok: true},
		{name: "zero", raw: `"0"`, want: `0`, ok: true},
		{name: "negative", raw: `-1`, ok: false},
		{name: "not numeric", raw: `"unknown"`, ok: false},
		{name: "object", raw: `{}`, ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usage := &Usage{Extra: map[string]json.RawMessage{"vendor": json.RawMessage(`true`)}}
			if got := AttachReportedCost(usage, json.RawMessage(tt.raw)); got != tt.ok {
				t.Fatalf("AttachReportedCost() = %v, want %v", got, tt.ok)
			}
			if !tt.ok {
				if _, exists := usage.Extra["cost"]; exists {
					t.Fatal("invalid cost was attached")
				}
				return
			}
			if got := string(usage.Extra["cost"]); got != tt.want {
				t.Errorf("usage cost = %s, want %s", got, tt.want)
			}
			if got := string(usage.Extra["vendor"]); got != "true" {
				t.Errorf("existing usage extra = %s, want true", got)
			}
		})
	}
}

func TestAttachUsageCost(t *testing.T) {
	rates := &ModelCost{Input: 1, Output: 2, CacheRead: 0.1, CacheWrite: 1.25}

	t.Run("positive provider cost wins", func(t *testing.T) {
		usage := &Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000}
		if got := AttachUsageCost(usage, json.RawMessage(`"0.25"`), rates); got != UsageCostProvider {
			t.Fatalf("source = %q, want provider", got)
		}
		if got := string(usage.Extra["cost"]); got != "0.25" {
			t.Fatalf("cost = %s, want 0.25", got)
		}
	})

	t.Run("zero provider cost falls back to estimate", func(t *testing.T) {
		usage := &Usage{PromptTokens: 1_000_000, CompletionTokens: 500_000}
		if got := AttachUsageCost(usage, json.RawMessage(`"0"`), rates); got != UsageCostCatalogEstimate {
			t.Fatalf("source = %q, want catalog_estimate", got)
		}
		if got := string(usage.Extra["cost"]); got != "2" {
			t.Fatalf("cost = %s, want 2", got)
		}
		if got := string(usage.Extra["cost_source"]); got != `"catalog_estimate"` {
			t.Fatalf("cost_source = %s, want catalog_estimate", got)
		}
	})

	t.Run("usage provider cost wins when top level is absent", func(t *testing.T) {
		usage := &Usage{
			PromptTokens: 1_000_000,
			Extra:        map[string]json.RawMessage{"cost": json.RawMessage(`"0.75"`)},
		}
		if got := AttachUsageCost(usage, nil, rates); got != UsageCostProvider {
			t.Fatalf("source = %q, want provider", got)
		}
		if got := string(usage.Extra["cost"]); got != "0.75" {
			t.Fatalf("cost = %s, want 0.75", got)
		}
	})

	t.Run("zero trailer keeps prior catalog estimate", func(t *testing.T) {
		usage := &Usage{
			PromptTokens: 1_000_000,
			Extra: map[string]json.RawMessage{
				"cost":        json.RawMessage(`1`),
				"cost_source": json.RawMessage(`"catalog_estimate"`),
			},
		}
		if got := AttachUsageCost(usage, json.RawMessage(`"0"`), rates); got != UsageCostCatalogEstimate {
			t.Fatalf("source = %q, want catalog_estimate", got)
		}
		if got := string(usage.Extra["cost_source"]); got != `"catalog_estimate"` {
			t.Fatalf("cost_source = %s, want catalog_estimate", got)
		}
	})

	t.Run("anthropic cache counters are additional input", func(t *testing.T) {
		usage := &Usage{
			PromptTokens:     100_000,
			CompletionTokens: 50_000,
			Extra: map[string]json.RawMessage{
				"cache_read_input_tokens":     json.RawMessage(`800000`),
				"cache_creation_input_tokens": json.RawMessage(`50000`),
			},
		}
		cost, ok := EstimateUsageCost(usage, rates)
		if !ok || cost != 0.3425 {
			t.Fatalf("EstimateUsageCost() = %v, %v; want 0.3425, true", cost, ok)
		}
	})

	t.Run("openai cached tokens are part of prompt", func(t *testing.T) {
		usage := &Usage{
			PromptTokens:     900_000,
			CompletionTokens: 50_000,
			Extra: map[string]json.RawMessage{
				"prompt_tokens_details": json.RawMessage(`{"cached_tokens":800000}`),
			},
		}
		cost, ok := EstimateUsageCost(usage, rates)
		if !ok || cost != 0.28 {
			t.Fatalf("EstimateUsageCost() = %v, %v; want 0.28, true", cost, ok)
		}
	})
}
