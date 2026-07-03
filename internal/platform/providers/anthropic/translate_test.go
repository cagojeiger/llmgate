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
