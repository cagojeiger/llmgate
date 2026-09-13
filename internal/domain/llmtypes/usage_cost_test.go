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
