package llmtypes

import (
	"encoding/json"
	"math"
	"strconv"
)

const tokensPerMillion = 1_000_000

// ModelCost contains USD rates per one million tokens. It is optional model
// metadata: adapters prefer a positive provider-reported total and use these
// rates only when the provider omits cost or reports zero.
type ModelCost struct {
	Input      float64 `yaml:"input"`
	Output     float64 `yaml:"output"`
	CacheRead  float64 `yaml:"cache_read,omitempty"`
	CacheWrite float64 `yaml:"cache_write,omitempty"`
}

type UsageCostSource string

const (
	UsageCostNone            UsageCostSource = ""
	UsageCostProvider        UsageCostSource = "provider"
	UsageCostCatalogEstimate UsageCostSource = "catalog_estimate"
)

// AttachReportedCost copies a provider-reported USD total into the usage
// extension understood by OpenAI-compatible consumers such as OpenClaw.
// Providers differ on whether they encode cost as a JSON number or string.
func AttachReportedCost(usage *Usage, raw json.RawMessage) bool {
	cost, ok := parseReportedCost(raw)
	if usage == nil || !ok {
		return false
	}
	return attachCost(usage, cost)
}

// AttachUsageCost normalizes a provider total into usage.cost. When the
// provider omits cost or reports zero, model rates provide a best-effort
// estimate so OpenAI-compatible consumers can still account for the call.
func AttachUsageCost(usage *Usage, raw json.RawMessage, rates *ModelCost) UsageCostSource {
	if usage == nil {
		return UsageCostNone
	}
	providerCost, reported := parseReportedCost(raw)
	if reported && providerCost > 0 {
		attachCost(usage, providerCost)
		delete(usage.Extra, "cost_source")
		return UsageCostProvider
	}
	usageCost, usageReported := parseReportedCost(usage.Extra["cost"])
	usageCostEstimated := string(usage.Extra["cost_source"]) == `"catalog_estimate"`
	if usageReported && usageCost > 0 && !usageCostEstimated {
		attachCost(usage, usageCost)
		delete(usage.Extra, "cost_source")
		return UsageCostProvider
	}

	if estimated, ok := EstimateUsageCost(usage, rates); ok && estimated > 0 {
		attachCost(usage, estimated)
		usage.Extra["cost_source"] = json.RawMessage(`"catalog_estimate"`)
		return UsageCostCatalogEstimate
	}

	if reported && AttachReportedCost(usage, raw) {
		return UsageCostProvider
	}
	if usageReported && !usageCostEstimated {
		return UsageCostProvider
	}
	return UsageCostNone
}

// EstimateUsageCost calculates a USD total from OpenAI-shaped usage. Direct
// Anthropic cache counters are separate from prompt_tokens; OpenAI's nested
// cached_tokens is included in prompt_tokens and is therefore subtracted from
// the full-rate input count.
func EstimateUsageCost(usage *Usage, rates *ModelCost) (float64, bool) {
	if usage == nil || rates == nil {
		return 0, false
	}

	inputTokens := usage.PromptTokens
	cacheRead, directRead := usageExtraInt(usage, "cache_read_input_tokens")
	cacheWrite, directWrite := usageExtraInt(usage, "cache_creation_input_tokens")
	if !directRead && !directWrite {
		if details, ok := usage.Extra["prompt_tokens_details"]; ok {
			var parsed struct {
				CachedTokens        int `json:"cached_tokens"`
				CacheWriteTokens    int `json:"cache_write_tokens"`
				CacheCreationTokens int `json:"cache_creation_input_tokens"`
			}
			if json.Unmarshal(details, &parsed) == nil {
				cacheRead = parsed.CachedTokens
				cacheWrite = parsed.CacheWriteTokens
				if cacheWrite == 0 {
					cacheWrite = parsed.CacheCreationTokens
				}
				inputTokens -= cacheRead + cacheWrite
				if inputTokens < 0 {
					inputTokens = 0
				}
			}
		}
	}

	cost := (float64(inputTokens)*rates.Input +
		float64(usage.CompletionTokens)*rates.Output +
		float64(cacheRead)*rates.CacheRead +
		float64(cacheWrite)*rates.CacheWrite) / tokensPerMillion
	if cost < 0 || math.IsNaN(cost) || math.IsInf(cost, 0) {
		return 0, false
	}
	return cost, true
}

func parseReportedCost(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}

	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false
	}

	var cost float64
	var err error
	switch v := value.(type) {
	case float64:
		cost = v
	case string:
		cost, err = strconv.ParseFloat(v, 64)
	default:
		return 0, false
	}
	if err != nil || cost < 0 || math.IsNaN(cost) || math.IsInf(cost, 0) {
		return 0, false
	}
	return cost, true
}

func attachCost(usage *Usage, cost float64) bool {
	normalized, err := json.Marshal(cost)
	if err != nil {
		return false
	}
	if usage.Extra == nil {
		usage.Extra = make(map[string]json.RawMessage)
	}
	usage.Extra["cost"] = normalized
	return true
}

func usageExtraInt(usage *Usage, key string) (int, bool) {
	if usage == nil {
		return 0, false
	}
	raw, ok := usage.Extra[key]
	if !ok {
		return 0, false
	}
	var value int
	if json.Unmarshal(raw, &value) != nil || value < 0 {
		return 0, false
	}
	return value, true
}
