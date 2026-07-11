package anthropic

import (
	"encoding/json"
	"testing"

	"llmgate/internal/domain/llmtypes"
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

func TestToAnthropicRequest_StructuredContentWithImage(t *testing.T) {
	got := decodeRequestBody(t, &llmtypes.Request{
		Model: "claude-x",
		Messages: []llmtypes.Message{{
			Role: "user",
			ContentRaw: json.RawMessage(`[
				{"type":"text","text":"what is this?"},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}},
				{"type":"image_url","image_url":{"url":"https://example.com/a.jpg"}}
			]`),
		}},
	})
	blocks := got["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(blocks) != 3 {
		t.Fatalf("content blocks = %d, want 3", len(blocks))
	}
	text := blocks[0].(map[string]any)
	if text["type"] != "text" || text["text"] != "what is this?" {
		t.Errorf("block 0 = %v, want text/'what is this?'", text)
	}
	b64 := blocks[1].(map[string]any)
	src := b64["source"].(map[string]any)
	if b64["type"] != "image" || src["type"] != "base64" ||
		src["media_type"] != "image/png" || src["data"] != "iVBORw0KGgo=" {
		t.Errorf("block 1 = %v, want base64 image/png source", b64)
	}
	urlBlock := blocks[2].(map[string]any)
	urlSrc := urlBlock["source"].(map[string]any)
	if urlSrc["type"] != "url" || urlSrc["url"] != "https://example.com/a.jpg" {
		t.Errorf("block 2 source = %v, want url source", urlSrc)
	}
}

func TestToAnthropicRequest_NullContentPreservesToolCalls(t *testing.T) {
	// Canonical OpenAI assistant tool-call turn: content is null. The message
	// UnmarshalJSON records that as ContentRaw "null", which must NOT be treated
	// as structured content or the tool_calls are silently dropped.
	var m llmtypes.Message
	if err := json.Unmarshal([]byte(
		`{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"seoul\"}"}}]}`,
	), &m); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}
	got := decodeRequestBody(t, &llmtypes.Request{
		Model:    "claude-x",
		Messages: []llmtypes.Message{{Role: "user", Content: "hi"}, m},
	})
	blocks := got["messages"].([]any)[1].(map[string]any)["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("assistant content blocks = %d, want 1", len(blocks))
	}
	b := blocks[0].(map[string]any)
	if b["type"] != "tool_use" || b["name"] != "get_weather" {
		t.Errorf("block = %v, want tool_use/get_weather", b)
	}
}

func TestToAnthropicRequest_ToolRoleArrayContent(t *testing.T) {
	// A tool-role message whose content is an array of text parts must still
	// become a tool_result block carrying tool_use_id, not a bare user turn.
	var m llmtypes.Message
	if err := json.Unmarshal([]byte(
		`{"role":"tool","tool_call_id":"call_1","content":[{"type":"text","text":"sunny 25C"}]}`,
	), &m); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}
	got := decodeRequestBody(t, &llmtypes.Request{
		Model:    "claude-x",
		Messages: []llmtypes.Message{{Role: "user", Content: "hi"}, m},
	})
	block := got["messages"].([]any)[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if block["type"] != "tool_result" || block["tool_use_id"] != "call_1" || block["content"] != "sunny 25C" {
		t.Errorf("block = %v, want tool_result/call_1/'sunny 25C'", block)
	}
}

func TestToAnthropicRequest_ArrayContentWithToolCalls(t *testing.T) {
	// An assistant turn with BOTH structured content and tool_calls must keep
	// both: content blocks first, then the tool_use block.
	var m llmtypes.Message
	if err := json.Unmarshal([]byte(
		`{"role":"assistant","content":[{"type":"text","text":"let me check"}],"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]}`,
	), &m); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}
	got := decodeRequestBody(t, &llmtypes.Request{
		Model:    "claude-x",
		Messages: []llmtypes.Message{{Role: "user", Content: "hi"}, m},
	})
	blocks := got["messages"].([]any)[1].(map[string]any)["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2 (text + tool_use)", len(blocks))
	}
	if blocks[0].(map[string]any)["type"] != "text" ||
		blocks[0].(map[string]any)["text"] != "let me check" {
		t.Errorf("block 0 = %v, want leading text", blocks[0])
	}
	if blocks[1].(map[string]any)["type"] != "tool_use" ||
		blocks[1].(map[string]any)["name"] != "get_weather" {
		t.Errorf("block 1 = %v, want tool_use/get_weather", blocks[1])
	}
}

func TestToAnthropicRequest_ArrayContentEmptyToolCalls(t *testing.T) {
	// content is an array but tool_calls is an empty []. The empty tool_calls
	// must not swallow the content — the blocks still have to be emitted.
	var m llmtypes.Message
	if err := json.Unmarshal([]byte(
		`{"role":"assistant","content":[{"type":"text","text":"hello there"}],"tool_calls":[]}`,
	), &m); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}
	got := decodeRequestBody(t, &llmtypes.Request{
		Model:    "claude-x",
		Messages: []llmtypes.Message{{Role: "user", Content: "hi"}, m},
	})
	block := got["messages"].([]any)[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "hello there" {
		t.Errorf("block = %v, want text/'hello there'", block)
	}
}

func TestToAnthropicRequest_EmptyContentArrayRejected(t *testing.T) {
	_, err := toAnthropicRequest(&llmtypes.Request{
		Model: "claude-x",
		Messages: []llmtypes.Message{{
			Role:       "user",
			ContentRaw: json.RawMessage(`[]`),
		}},
	}, 32, false)
	if err == nil {
		t.Fatal("expected error for empty content array, got nil")
	}
}

func TestToAnthropicRequest_DataURIMediaTypeParams(t *testing.T) {
	// A data URI with media-type parameters must yield a bare media_type.
	got := decodeRequestBody(t, &llmtypes.Request{
		Model: "claude-x",
		Messages: []llmtypes.Message{{
			Role:       "user",
			ContentRaw: json.RawMessage(`[{"type":"image_url","image_url":{"url":"data:image/png;charset=utf-8;base64,iVBOR\nw0KGgo="}}]`),
		}},
	})
	src := got["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["source"].(map[string]any)
	if src["media_type"] != "image/png" {
		t.Errorf("media_type = %v, want image/png (params stripped)", src["media_type"])
	}
	if src["data"] != "iVBORw0KGgo=" {
		t.Errorf("data = %v, want newline stripped", src["data"])
	}
}

func TestToAnthropicRequest_MalformedDataURI(t *testing.T) {
	_, err := toAnthropicRequest(&llmtypes.Request{
		Model: "claude-x",
		Messages: []llmtypes.Message{{
			Role:       "user",
			ContentRaw: json.RawMessage(`[{"type":"image_url","image_url":{"url":"data:image/png,notbase64"}}]`),
		}},
	}, 32, false)
	if err == nil {
		t.Fatal("expected error for non-base64 data URI, got nil")
	}
}

func TestToAnthropicRequest_DropsOpenAIOnlyKeys(t *testing.T) {
	// OpenAI-only params must not reach the Anthropic wire (a strict upstream
	// 400s on them, and a 400 is not fallback-eligible), while anthropic-native
	// extras like top_k keep passing through.
	got := decodeRequestBody(t, &llmtypes.Request{
		Model:    "claude-x",
		Messages: []llmtypes.Message{{Role: "user", Content: "hi"}},
		Extra: map[string]json.RawMessage{
			"frequency_penalty": json.RawMessage(`0.5`),
			"response_format":   json.RawMessage(`{"type":"json_object"}`),
			"metadata":          json.RawMessage(`{"team":"infra"}`),
			"logit_bias":        json.RawMessage(`{"1234":5}`),
			"top_k":             json.RawMessage(`40`),
		},
	})
	for _, key := range []string{"frequency_penalty", "response_format", "metadata", "logit_bias"} {
		if _, leaked := got[key]; leaked {
			t.Errorf("%s leaked onto the anthropic wire", key)
		}
	}
	if got["top_k"] != float64(40) {
		t.Errorf("top_k = %v, want 40 (anthropic-native extras must pass through)", got["top_k"])
	}
}

func TestToAnthropicRequest_MaxCompletionTokens(t *testing.T) {
	// Newer OpenAI SDKs send max_completion_tokens; it must become max_tokens
	// instead of leaking as an unknown param.
	got := decodeRequestBody(t, &llmtypes.Request{
		Model:    "claude-x",
		Messages: []llmtypes.Message{{Role: "user", Content: "hi"}},
		Extra: map[string]json.RawMessage{
			"max_completion_tokens": json.RawMessage(`123`),
		},
	})
	if got["max_tokens"] != float64(123) {
		t.Errorf("max_tokens = %v, want 123", got["max_tokens"])
	}
	if _, leaked := got["max_completion_tokens"]; leaked {
		t.Error("max_completion_tokens leaked onto the anthropic wire")
	}

	// An explicit typed max_tokens wins over max_completion_tokens.
	got = decodeRequestBody(t, &llmtypes.Request{
		Model:     "claude-x",
		MaxTokens: 77,
		Messages:  []llmtypes.Message{{Role: "user", Content: "hi"}},
		Extra: map[string]json.RawMessage{
			"max_completion_tokens": json.RawMessage(`123`),
		},
	})
	if got["max_tokens"] != float64(77) {
		t.Errorf("max_tokens = %v, want 77 (typed field wins)", got["max_tokens"])
	}
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
