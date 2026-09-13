package llmtypes

import (
	"encoding/json"
	"math"
	"strconv"
)

// AttachReportedCost copies a provider-reported USD total into the usage
// extension understood by OpenAI-compatible consumers such as OpenClaw.
// Providers differ on whether they encode cost as a JSON number or string.
func AttachReportedCost(usage *Usage, raw json.RawMessage) bool {
	if usage == nil || len(raw) == 0 {
		return false
	}

	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}

	var cost float64
	var err error
	switch v := value.(type) {
	case float64:
		cost = v
	case string:
		cost, err = strconv.ParseFloat(v, 64)
	default:
		return false
	}
	if err != nil || cost < 0 || math.IsNaN(cost) || math.IsInf(cost, 0) {
		return false
	}

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
