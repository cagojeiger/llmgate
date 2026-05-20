package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"llmgate/internal/domain/llmtypes"
)

// strPtr returns a pointer to the given string for fields like StopReason
// that are *string in anthropicResponse.
func strPtr(s string) *string { return &s }

func decodeToolCalls(t *testing.T, msg llmtypes.Message) []map[string]any {
	t.Helper()
	raw, ok := msg.Extra["tool_calls"]
	if !ok {
		return nil
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode tool_calls: %v", err)
	}
	return out
}

func TestToOpenAIResponse_PlainText(t *testing.T) {
	resp, err := toOpenAIResponse(&anthropicResponse{
		ID:    "msg-1",
		Model: "claude-x",
		Content: []anthropicContent{
			{Type: "text", Text: "hello"},
		},
		StopReason: strPtr("end_turn"),
	})
	if err != nil {
		t.Fatalf("toOpenAIResponse error = %v", err)
	}
	msg := resp.Choices[0].Message
	if msg.Content != "hello" {
		t.Errorf("content = %q, want hello", msg.Content)
	}
	if calls := decodeToolCalls(t, msg); calls != nil {
		t.Errorf("tool_calls should be absent, got %v", calls)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", resp.Choices[0].FinishReason)
	}
}

func TestToOpenAIResponse_SingleToolUse(t *testing.T) {
	resp, err := toOpenAIResponse(&anthropicResponse{
		ID:    "msg-1",
		Model: "claude-x",
		Content: []anthropicContent{
			{Type: "tool_use", ID: "call-1", Name: "get_time", Input: json.RawMessage(`{"tz":"UTC"}`)},
		},
		StopReason: strPtr("tool_use"),
	})
	if err != nil {
		t.Fatalf("toOpenAIResponse error = %v", err)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", resp.Choices[0].FinishReason)
	}
	calls := decodeToolCalls(t, resp.Choices[0].Message)
	if len(calls) != 1 {
		t.Fatalf("calls len = %d, want 1", len(calls))
	}
	c := calls[0]
	if c["id"] != "call-1" || c["type"] != "function" {
		t.Errorf("call header = %v", c)
	}
	function := c["function"].(map[string]any)
	if function["name"] != "get_time" {
		t.Errorf("function.name = %q, want get_time", function["name"])
	}
	args, ok := function["arguments"].(string)
	if !ok {
		t.Fatalf("arguments must be a string (OpenAI shape), got %T", function["arguments"])
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		t.Fatalf("arguments must be valid JSON: %v", err)
	}
	if parsed["tz"] != "UTC" {
		t.Errorf("decoded args = %v, want {tz:UTC}", parsed)
	}
}

func TestToOpenAIResponse_MultipleToolUseOrderPreserved(t *testing.T) {
	resp, err := toOpenAIResponse(&anthropicResponse{
		ID:    "msg-1",
		Model: "claude-x",
		Content: []anthropicContent{
			{Type: "tool_use", ID: "a", Name: "first", Input: json.RawMessage(`{}`)},
			{Type: "tool_use", ID: "b", Name: "second", Input: json.RawMessage(`{}`)},
			{Type: "tool_use", ID: "c", Name: "third", Input: json.RawMessage(`{}`)},
		},
		StopReason: strPtr("tool_use"),
	})
	if err != nil {
		t.Fatalf("toOpenAIResponse error = %v", err)
	}
	calls := decodeToolCalls(t, resp.Choices[0].Message)
	if len(calls) != 3 {
		t.Fatalf("calls len = %d, want 3", len(calls))
	}
	want := []string{"a", "b", "c"}
	for i, c := range calls {
		if c["id"] != want[i] {
			t.Errorf("calls[%d].id = %q, want %q", i, c["id"], want[i])
		}
	}
}

func TestToOpenAIResponse_MixedTextAndToolUse(t *testing.T) {
	resp, err := toOpenAIResponse(&anthropicResponse{
		ID:    "msg-1",
		Model: "claude-x",
		Content: []anthropicContent{
			{Type: "text", Text: "let me check"},
			{Type: "tool_use", ID: "call-1", Name: "lookup", Input: json.RawMessage(`{"q":"x"}`)},
		},
		StopReason: strPtr("tool_use"),
	})
	if err != nil {
		t.Fatalf("toOpenAIResponse error = %v", err)
	}
	msg := resp.Choices[0].Message
	if msg.Content != "let me check" {
		t.Errorf("content = %q, want \"let me check\"", msg.Content)
	}
	calls := decodeToolCalls(t, msg)
	if len(calls) != 1 || calls[0]["id"] != "call-1" {
		t.Errorf("tool_calls = %v, want one call call-1", calls)
	}
}

func TestToOpenAIResponse_EmptyInputDefaultsToObjectString(t *testing.T) {
	resp, err := toOpenAIResponse(&anthropicResponse{
		ID:    "msg-1",
		Model: "claude-x",
		Content: []anthropicContent{
			{Type: "tool_use", ID: "x", Name: "noop"},
		},
		StopReason: strPtr("tool_use"),
	})
	if err != nil {
		t.Fatalf("toOpenAIResponse error = %v", err)
	}
	calls := decodeToolCalls(t, resp.Choices[0].Message)
	if len(calls) != 1 {
		t.Fatalf("calls len = %d, want 1", len(calls))
	}
	args := calls[0]["function"].(map[string]any)["arguments"].(string)
	if args != "{}" {
		t.Errorf("empty input arguments = %q, want \"{}\"", args)
	}
}

func TestToOpenAIResponse_ToolUseWithoutNameSkipped(t *testing.T) {
	resp, err := toOpenAIResponse(&anthropicResponse{
		ID:    "msg-1",
		Model: "claude-x",
		Content: []anthropicContent{
			{Type: "tool_use", ID: "no-name"},
			{Type: "tool_use", ID: "ok", Name: "good", Input: json.RawMessage(`{}`)},
		},
		StopReason: strPtr("tool_use"),
	})
	if err != nil {
		t.Fatalf("toOpenAIResponse error = %v", err)
	}
	calls := decodeToolCalls(t, resp.Choices[0].Message)
	if len(calls) != 1 || calls[0]["id"] != "ok" {
		t.Errorf("expected only the named tool_use, got %v", calls)
	}
}

func TestToOpenAIResponse_MarshalledMessageIncludesToolCalls(t *testing.T) {
	// Through the full Message MarshalJSON path so we verify wire shape.
	resp, err := toOpenAIResponse(&anthropicResponse{
		ID:    "msg-1",
		Model: "claude-x",
		Content: []anthropicContent{
			{Type: "tool_use", ID: "call-1", Name: "ping", Input: json.RawMessage(`{}`)},
		},
		StopReason: strPtr("tool_use"),
	})
	if err != nil {
		t.Fatalf("toOpenAIResponse error = %v", err)
	}
	body, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	if !strings.Contains(string(body), `"tool_calls":[`) {
		t.Errorf("marshaled response is missing tool_calls field at the message level: %s", body)
	}
}
