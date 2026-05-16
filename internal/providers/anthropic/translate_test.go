package anthropic

import (
	"encoding/json"
	"testing"

	"llmgate/internal/llmtypes"
)

// decodeRequestBody marshals a Request through toAnthropicRequest and
// returns the resulting wire JSON as a generic map for assertion.
func decodeRequestBody(t *testing.T, req *llmtypes.Request) map[string]any {
	t.Helper()
	body, err := toAnthropicRequest(req, 32, false)
	if err != nil {
		t.Fatalf("toAnthropicRequest error = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	return got
}

func TestToAnthropicRequest_PlainText(t *testing.T) {
	got := decodeRequestBody(t, &llmtypes.Request{
		Model:    "claude-x",
		Messages: []llmtypes.Message{{Role: "user", Content: "hi"}},
	})
	msgs := got["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages len = %d, want 1", len(msgs))
	}
	m := msgs[0].(map[string]any)
	if m["role"] != "user" {
		t.Errorf("role = %q, want user", m["role"])
	}
	if m["content"] != "hi" {
		t.Errorf("content = %v, want \"hi\"", m["content"])
	}
	if _, has := got["tools"]; has {
		t.Errorf("tools must be omitted when none provided")
	}
	if _, has := got["tool_choice"]; has {
		t.Errorf("tool_choice must be omitted when none provided")
	}
}
