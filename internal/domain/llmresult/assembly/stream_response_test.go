package assembly

import (
	"encoding/json"
	"testing"

	"llmgate/internal/domain/llmtypes"
)

func TestStreamResponseBuilder_AssemblesTextAndReasoning(t *testing.T) {
	b := NewStreamResponseBuilder()
	b.Add(&llmtypes.Event{
		ID:      "chatcmpl-1",
		Object:  "chat.completion.chunk",
		Created: 1700000000,
		Model:   "deepseek-v4-flash",
		Choices: []llmtypes.ChoiceDelta{{
			Index: 0,
			Delta: llmtypes.Delta{Role: "assistant"},
		}},
	})
	b.Add(&llmtypes.Event{
		Model: "deepseek-v4-flash",
		Choices: []llmtypes.ChoiceDelta{{
			Index: 0,
			Delta: llmtypes.Delta{ReasoningContent: "because "},
		}},
	})
	b.Add(&llmtypes.Event{
		Model: "deepseek-v4-flash",
		Choices: []llmtypes.ChoiceDelta{{
			Index: 0,
			Delta: llmtypes.Delta{Content: "hel"},
		}},
	})
	b.Add(&llmtypes.Event{
		Model: "deepseek-v4-flash",
		Choices: []llmtypes.ChoiceDelta{{
			Index:        0,
			Delta:        llmtypes.Delta{Content: "lo"},
			FinishReason: "stop",
		}},
		Usage: &llmtypes.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5},
	})

	resp := b.Response()
	if resp.ID != "chatcmpl-1" || resp.Object != "chat.completion" || resp.Model != "deepseek-v4-flash" {
		t.Fatalf("metadata = %+v", resp)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(resp.Choices))
	}
	msg := resp.Choices[0].Message
	if msg.Role != "assistant" {
		t.Errorf("role = %q, want assistant", msg.Role)
	}
	if msg.ReasoningContent != "because " {
		t.Errorf("reasoning_content = %q, want because ", msg.ReasoningContent)
	}
	if msg.Content != "hello" {
		t.Errorf("content = %q, want hello", msg.Content)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", resp.Choices[0].FinishReason)
	}
	if resp.Usage.TotalTokens != 5 {
		t.Errorf("total_tokens = %d, want 5", resp.Usage.TotalTokens)
	}
}

func TestStreamResponseBuilder_AssemblesToolCalls(t *testing.T) {
	b := NewStreamResponseBuilder()
	b.Add(&llmtypes.Event{
		ID:    "chatcmpl-tool",
		Model: "claude",
		Choices: []llmtypes.ChoiceDelta{{
			Index: 0,
			Delta: llmtypes.Delta{
				Role: "assistant",
				Extra: map[string]json.RawMessage{
					"tool_calls": json.RawMessage(`[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":"}}]`),
				},
			},
		}},
	})
	b.Add(&llmtypes.Event{
		Choices: []llmtypes.ChoiceDelta{{
			Index: 0,
			Delta: llmtypes.Delta{
				Extra: map[string]json.RawMessage{
					"tool_calls": json.RawMessage(`[{"index":0,"function":{"arguments":"\"weather\"}"}}]`),
				},
			},
		}},
	})
	b.Add(&llmtypes.Event{
		Choices: []llmtypes.ChoiceDelta{{
			Index:        0,
			FinishReason: "tool_calls",
		}},
	})

	resp := b.Response()
	msg := resp.Choices[0].Message
	raw := msg.Extra["tool_calls"]
	if len(raw) == 0 {
		t.Fatal("tool_calls missing from final message extra")
	}
	var calls []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &calls); err != nil {
		t.Fatalf("unmarshal tool_calls: %v; raw=%s", err, raw)
	}
	if len(calls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(calls))
	}
	if calls[0].ID != "call_1" || calls[0].Type != "function" || calls[0].Function.Name != "lookup" {
		t.Fatalf("tool call metadata = %+v", calls[0])
	}
	if calls[0].Function.Arguments != `{"q":"weather"}` {
		t.Errorf("arguments = %q, want JSON object string", calls[0].Function.Arguments)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", resp.Choices[0].FinishReason)
	}
}

func TestStreamResponseBuilder_OrdersChoicesByIndex(t *testing.T) {
	b := NewStreamResponseBuilder()
	b.Add(&llmtypes.Event{Choices: []llmtypes.ChoiceDelta{
		{Index: 2, Delta: llmtypes.Delta{Content: "two"}},
		{Index: 0, Delta: llmtypes.Delta{Content: "zero"}},
		{Index: 1, Delta: llmtypes.Delta{Content: "one"}},
	}})

	resp := b.Response()
	if len(resp.Choices) != 3 {
		t.Fatalf("choices = %d, want 3", len(resp.Choices))
	}
	for i, choice := range resp.Choices {
		if choice.Index != i {
			t.Fatalf("choice[%d].Index = %d, want %d", i, choice.Index, i)
		}
	}
}
