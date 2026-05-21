package assembly

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"llmgate/internal/domain/llmtypes"
)

// BenchmarkStreamAssembly_Content models a typical OpenAI chat
// streaming response: many small content deltas, no reasoning, no
// tool calls. This is the dominant shape for plain chat traffic.
func BenchmarkStreamAssembly_Content(b *testing.B) {
	deltas := makeContentDeltas(200, "lorem ipsum dolor ")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		builder := NewStreamResponseBuilder()
		for _, e := range deltas {
			builder.Add(e)
		}
		_ = builder.Response()
	}
}

// BenchmarkStreamAssembly_Reasoning models DeepSeek-style responses:
// reasoning_content streams alongside or before final content, often
// producing KB-scale accumulated text. This is the worst case for
// per-delta string concatenation.
func BenchmarkStreamAssembly_Reasoning(b *testing.B) {
	deltas := makeReasoningDeltas(300, "thinking step by step: ")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		builder := NewStreamResponseBuilder()
		for _, e := range deltas {
			builder.Add(e)
		}
		_ = builder.Response()
	}
}

// BenchmarkStreamAssembly_ToolCalls models tool-call streaming where
// function arguments arrive delta-by-delta. The tool_call payload is
// carried in Delta.Extra["tool_calls"] as json.RawMessage, which the
// builder must json.Unmarshal on every event — this is the per-event
// cost the original audit flagged as candidate B.
func BenchmarkStreamAssembly_ToolCalls(b *testing.B) {
	deltas := makeToolCallDeltas(100)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		builder := NewStreamResponseBuilder()
		for _, e := range deltas {
			builder.Add(e)
		}
		_ = builder.Response()
	}
}

func makeContentDeltas(n int, chunk string) []*llmtypes.Event {
	events := make([]*llmtypes.Event, 0, n+1)
	events = append(events, &llmtypes.Event{
		ID:      "chatcmpl-bench",
		Object:  "chat.completion.chunk",
		Created: 1700000000,
		Model:   "gpt-5o-mini",
		Choices: []llmtypes.ChoiceDelta{{
			Index: 0,
			Delta: llmtypes.Delta{Role: "assistant"},
		}},
	})
	for i := 0; i < n; i++ {
		events = append(events, &llmtypes.Event{
			Model: "gpt-5o-mini",
			Choices: []llmtypes.ChoiceDelta{{
				Index: 0,
				Delta: llmtypes.Delta{Content: chunk},
			}},
		})
	}
	events = append(events, &llmtypes.Event{
		Model: "gpt-5o-mini",
		Choices: []llmtypes.ChoiceDelta{{
			Index:        0,
			Delta:        llmtypes.Delta{},
			FinishReason: "stop",
		}},
		Usage: &llmtypes.Usage{PromptTokens: 10, CompletionTokens: n, TotalTokens: 10 + n},
	})
	return events
}

func makeReasoningDeltas(n int, chunk string) []*llmtypes.Event {
	events := make([]*llmtypes.Event, 0, n+2)
	events = append(events, &llmtypes.Event{
		ID:      "chatcmpl-bench",
		Object:  "chat.completion.chunk",
		Created: 1700000000,
		Model:   "deepseek-v4-thinking",
		Choices: []llmtypes.ChoiceDelta{{
			Index: 0,
			Delta: llmtypes.Delta{Role: "assistant"},
		}},
	})
	for i := 0; i < n; i++ {
		events = append(events, &llmtypes.Event{
			Model: "deepseek-v4-thinking",
			Choices: []llmtypes.ChoiceDelta{{
				Index: 0,
				Delta: llmtypes.Delta{ReasoningContent: chunk},
			}},
		})
	}
	// Final content chunks (small) and finish.
	events = append(events, &llmtypes.Event{
		Model: "deepseek-v4-thinking",
		Choices: []llmtypes.ChoiceDelta{{
			Index: 0,
			Delta: llmtypes.Delta{Content: "final answer"},
		}},
	})
	events = append(events, &llmtypes.Event{
		Model: "deepseek-v4-thinking",
		Choices: []llmtypes.ChoiceDelta{{
			Index:        0,
			Delta:        llmtypes.Delta{},
			FinishReason: "stop",
		}},
		Usage: &llmtypes.Usage{PromptTokens: 12, CompletionTokens: n + 2, TotalTokens: 14 + n},
	})
	return events
}

func makeToolCallDeltas(n int) []*llmtypes.Event {
	events := make([]*llmtypes.Event, 0, n+2)
	events = append(events, &llmtypes.Event{
		ID:      "chatcmpl-bench",
		Object:  "chat.completion.chunk",
		Created: 1700000000,
		Model:   "gpt-5o",
		Choices: []llmtypes.ChoiceDelta{{
			Index: 0,
			Delta: llmtypes.Delta{Role: "assistant"},
		}},
	})
	// First tool-call delta carries id/type/name; subsequent ones
	// stream the arguments JSON delta-by-delta.
	openDelta := []map[string]any{{
		"index": 0,
		"id":    "call_abc123",
		"type":  "function",
		"function": map[string]any{
			"name":      "lookup_city",
			"arguments": `{"city":"`,
		},
	}}
	openRaw, _ := json.Marshal(openDelta)
	events = append(events, &llmtypes.Event{
		Model: "gpt-5o",
		Choices: []llmtypes.ChoiceDelta{{
			Index: 0,
			Delta: llmtypes.Delta{Extra: map[string]json.RawMessage{
				"tool_calls": openRaw,
			}},
		}},
	})
	for i := 0; i < n; i++ {
		argChunk := `"` + strings.Repeat("x", 4) + strconv.Itoa(i) + `",`
		if i == n-1 {
			argChunk = `"end"}`
		}
		argDelta := []map[string]any{{
			"index": 0,
			"function": map[string]any{
				"arguments": argChunk,
			},
		}}
		raw, _ := json.Marshal(argDelta)
		events = append(events, &llmtypes.Event{
			Model: "gpt-5o",
			Choices: []llmtypes.ChoiceDelta{{
				Index: 0,
				Delta: llmtypes.Delta{Extra: map[string]json.RawMessage{
					"tool_calls": raw,
				}},
			}},
		})
	}
	events = append(events, &llmtypes.Event{
		Model: "gpt-5o",
		Choices: []llmtypes.ChoiceDelta{{
			Index:        0,
			Delta:        llmtypes.Delta{},
			FinishReason: "tool_calls",
		}},
		Usage: &llmtypes.Usage{PromptTokens: 20, CompletionTokens: n, TotalTokens: 20 + n},
	})
	return events
}
