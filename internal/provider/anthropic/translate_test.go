package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"llmgate/internal/provider"
)

// decodeRequestBody marshals a Request through toAnthropicRequest and
// returns the resulting wire JSON as a generic map for assertion.
func decodeRequestBody(t *testing.T, req *provider.Request) map[string]any {
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
	got := decodeRequestBody(t, &provider.Request{
		Model:    "claude-x",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
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

func TestToAnthropicRequest_ToolsBasic(t *testing.T) {
	got := decodeRequestBody(t, &provider.Request{
		Model:    "claude-x",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
		Extra: map[string]json.RawMessage{
			"tools": json.RawMessage(`[
				{"type":"function","function":{"name":"get_time","description":"return current time","parameters":{"type":"object","properties":{"tz":{"type":"string"}},"required":["tz"]}}}
			]`),
		},
	})
	tools, ok := got["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v, want one element", got["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "get_time" {
		t.Errorf("tool.name = %q, want get_time", tool["name"])
	}
	if tool["description"] != "return current time" {
		t.Errorf("tool.description = %q, want return current time", tool["description"])
	}
	schema, ok := tool["input_schema"].(map[string]any)
	if !ok {
		t.Fatalf("input_schema missing or wrong type: %T", tool["input_schema"])
	}
	if schema["type"] != "object" {
		t.Errorf("input_schema.type = %q, want object", schema["type"])
	}
	if _, has := tool["parameters"]; has {
		t.Errorf("anthropic tool must not carry OpenAI's parameters key")
	}
}

func TestToAnthropicRequest_ToolsEmptyParametersDefaultsToObject(t *testing.T) {
	got := decodeRequestBody(t, &provider.Request{
		Model:    "claude-x",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
		Extra: map[string]json.RawMessage{
			"tools": json.RawMessage(`[{"type":"function","function":{"name":"ping"}}]`),
		},
	})
	tools := got["tools"].([]any)
	tool := tools[0].(map[string]any)
	schema := tool["input_schema"].(map[string]any)
	if schema["type"] != "object" {
		t.Errorf("default schema.type = %q, want object", schema["type"])
	}
	if props, ok := schema["properties"].(map[string]any); !ok || len(props) != 0 {
		t.Errorf("default schema.properties = %v, want empty object", schema["properties"])
	}
}

func TestToAnthropicRequest_ToolsRejectsNonObjectSchema(t *testing.T) {
	_, err := toAnthropicRequest(&provider.Request{
		Model:    "claude-x",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
		Extra: map[string]json.RawMessage{
			"tools": json.RawMessage(`[{"type":"function","function":{"name":"odd","parameters":{"type":"string"}}}]`),
		},
	}, 32, false)
	if err == nil || !strings.Contains(err.Error(), "object schema") {
		t.Fatalf("error = %v, want object-schema error", err)
	}
}

func TestToAnthropicRequest_ToolsRejectsUnsupportedType(t *testing.T) {
	_, err := toAnthropicRequest(&provider.Request{
		Model:    "claude-x",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
		Extra: map[string]json.RawMessage{
			"tools": json.RawMessage(`[{"type":"web_search"}]`),
		},
	}, 32, false)
	if err == nil || !strings.Contains(err.Error(), "tool type") {
		t.Fatalf("error = %v, want unsupported-tool-type error", err)
	}
}

func TestToAnthropicRequest_ToolChoiceVariants(t *testing.T) {
	cases := []struct {
		label    string
		raw      string
		wantType string
		wantName string
		wantNone bool
	}{
		{"auto-string", `"auto"`, "auto", "", false},
		{"required-string", `"required"`, "any", "", false},
		{"none-string", `"none"`, "", "", true},
		{"empty-string", `""`, "auto", "", false},
		{"object-auto", `{"type":"auto"}`, "auto", "", false},
		{"object-any", `{"type":"any"}`, "any", "", false},
		{"object-function", `{"type":"function","function":{"name":"pick"}}`, "tool", "pick", false},
		{"object-tool-direct-name", `{"type":"tool","name":"pick"}`, "tool", "pick", false},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			body, err := toAnthropicRequest(&provider.Request{
				Model:    "claude-x",
				Messages: []provider.Message{{Role: "user", Content: "hi"}},
				Extra: map[string]json.RawMessage{
					"tools":       json.RawMessage(`[{"type":"function","function":{"name":"pick"}}]`),
					"tool_choice": json.RawMessage(tc.raw),
				},
			}, 32, false)
			if err != nil {
				t.Fatalf("toAnthropicRequest error = %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if tc.wantNone {
				if _, has := got["tools"]; has {
					t.Errorf("tools should be dropped on tool_choice=none")
				}
				if _, has := got["tool_choice"]; has {
					t.Errorf("tool_choice should be dropped on tool_choice=none")
				}
				return
			}
			choice, ok := got["tool_choice"].(map[string]any)
			if !ok {
				t.Fatalf("tool_choice missing or wrong type: %T", got["tool_choice"])
			}
			if choice["type"] != tc.wantType {
				t.Errorf("type = %q, want %q", choice["type"], tc.wantType)
			}
			if tc.wantName != "" && choice["name"] != tc.wantName {
				t.Errorf("name = %q, want %q", choice["name"], tc.wantName)
			}
		})
	}
}

func TestToAnthropicRequest_ParallelToolCallsFalse(t *testing.T) {
	got := decodeRequestBody(t, &provider.Request{
		Model:    "claude-x",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
		Extra: map[string]json.RawMessage{
			"tools":               json.RawMessage(`[{"type":"function","function":{"name":"pick"}}]`),
			"parallel_tool_calls": json.RawMessage(`false`),
		},
	})
	choice, ok := got["tool_choice"].(map[string]any)
	if !ok {
		t.Fatalf("tool_choice should be auto-injected when parallel_tool_calls=false")
	}
	if choice["type"] != "auto" {
		t.Errorf("type = %q, want auto", choice["type"])
	}
	if disable, _ := choice["disable_parallel_tool_use"].(bool); !disable {
		t.Errorf("disable_parallel_tool_use = %v, want true", choice["disable_parallel_tool_use"])
	}
}

func TestToAnthropicRequest_ParallelToolCallsTrueLeavesUnset(t *testing.T) {
	got := decodeRequestBody(t, &provider.Request{
		Model:    "claude-x",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
		Extra: map[string]json.RawMessage{
			"tools":               json.RawMessage(`[{"type":"function","function":{"name":"pick"}}]`),
			"parallel_tool_calls": json.RawMessage(`true`),
		},
	})
	if _, has := got["tool_choice"]; has {
		t.Errorf("tool_choice should not be auto-set when parallel_tool_calls=true")
	}
}

func TestToAnthropicRequest_AssistantWithToolCalls(t *testing.T) {
	got := decodeRequestBody(t, &provider.Request{
		Model: "claude-x",
		Messages: []provider.Message{
			{Role: "user", Content: "what time?"},
			{
				Role:    "assistant",
				Content: "let me check",
				Extra: map[string]json.RawMessage{
					"tool_calls": json.RawMessage(`[{"id":"call-1","type":"function","function":{"name":"get_time","arguments":"{\"tz\":\"UTC\"}"}}]`),
				},
			},
		},
	})
	msgs := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages len = %d, want 2", len(msgs))
	}
	asst := msgs[1].(map[string]any)
	blocks, ok := asst["content"].([]any)
	if !ok {
		t.Fatalf("assistant content should be array of blocks, got %T", asst["content"])
	}
	if len(blocks) != 2 {
		t.Fatalf("blocks len = %d, want 2 (text + tool_use)", len(blocks))
	}
	textBlock := blocks[0].(map[string]any)
	if textBlock["type"] != "text" || textBlock["text"] != "let me check" {
		t.Errorf("text block = %v", textBlock)
	}
	useBlock := blocks[1].(map[string]any)
	if useBlock["type"] != "tool_use" || useBlock["id"] != "call-1" || useBlock["name"] != "get_time" {
		t.Errorf("tool_use block = %v", useBlock)
	}
	input, ok := useBlock["input"].(map[string]any)
	if !ok || input["tz"] != "UTC" {
		t.Errorf("input = %v, want {tz: UTC}", useBlock["input"])
	}
}

func TestToAnthropicRequest_AssistantToolCallsWithEmptyArgs(t *testing.T) {
	got := decodeRequestBody(t, &provider.Request{
		Model: "claude-x",
		Messages: []provider.Message{
			{Role: "user", Content: "go"},
			{
				Role: "assistant",
				Extra: map[string]json.RawMessage{
					"tool_calls": json.RawMessage(`[{"id":"c","type":"function","function":{"name":"noop","arguments":""}}]`),
				},
			},
		},
	})
	asst := got["messages"].([]any)[1].(map[string]any)
	blocks := asst["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("blocks len = %d, want 1 (only tool_use, no text)", len(blocks))
	}
	use := blocks[0].(map[string]any)
	input, ok := use["input"].(map[string]any)
	if !ok || len(input) != 0 {
		t.Errorf("empty arguments must default to empty object, got %v", use["input"])
	}
}

func TestToAnthropicRequest_ToolMessage(t *testing.T) {
	got := decodeRequestBody(t, &provider.Request{
		Model: "claude-x",
		Messages: []provider.Message{
			{Role: "user", Content: "go"},
			{Role: "assistant", Content: "calling", Extra: map[string]json.RawMessage{
				"tool_calls": json.RawMessage(`[{"id":"call-1","type":"function","function":{"name":"f","arguments":"{}"}}]`),
			}},
			{Role: "tool", Content: "12:34Z", Extra: map[string]json.RawMessage{
				"tool_call_id": json.RawMessage(`"call-1"`),
			}},
		},
	})
	msgs := got["messages"].([]any)
	tool := msgs[2].(map[string]any)
	if tool["role"] != "user" {
		t.Errorf("tool message role = %q, want user (anthropic has no tool role)", tool["role"])
	}
	blocks, ok := tool["content"].([]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("tool content = %v", tool["content"])
	}
	block := blocks[0].(map[string]any)
	if block["type"] != "tool_result" || block["tool_use_id"] != "call-1" || block["content"] != "12:34Z" {
		t.Errorf("tool_result block = %v", block)
	}
}

func TestToAnthropicRequest_ToolMessageMissingID(t *testing.T) {
	_, err := toAnthropicRequest(&provider.Request{
		Model: "claude-x",
		Messages: []provider.Message{
			{Role: "user", Content: "go"},
			{Role: "tool", Content: "12:34Z"},
		},
	}, 32, false)
	if err == nil || !strings.Contains(err.Error(), "tool_call_id") {
		t.Fatalf("error = %v, want missing-tool_call_id error", err)
	}
}

func TestToAnthropicRequest_PreservesUnrelatedExtra(t *testing.T) {
	got := decodeRequestBody(t, &provider.Request{
		Model:    "claude-x",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
		Extra: map[string]json.RawMessage{
			"vendor_request": json.RawMessage(`"keep"`),
			"tools":          json.RawMessage(`[{"type":"function","function":{"name":"pick"}}]`),
		},
	})
	if got["vendor_request"] != "keep" {
		t.Errorf("vendor_request = %v, want kept passthrough", got["vendor_request"])
	}
	if _, has := got["tools"]; !has {
		t.Errorf("tools should be present after translation")
	}
}

func TestToAnthropicRequest_DropsConsumedExtraOnError_None(t *testing.T) {
	// tool_choice=none must drop tools AND tool_choice from the wire even
	// though Extra had them — verifying the Extra-merge does not leak the
	// original OpenAI-shaped values back.
	got := decodeRequestBody(t, &provider.Request{
		Model:    "claude-x",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
		Extra: map[string]json.RawMessage{
			"tools":       json.RawMessage(`[{"type":"function","function":{"name":"pick"}}]`),
			"tool_choice": json.RawMessage(`"none"`),
		},
	})
	if _, has := got["tools"]; has {
		t.Errorf("tools must be dropped on tool_choice=none, body=%v", got)
	}
	if _, has := got["tool_choice"]; has {
		t.Errorf("tool_choice must be dropped on tool_choice=none, body=%v", got)
	}
}
