package anthropic

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"llmgate/internal/domain/llmtypes"
)

// streamToolCallAcc replays the OpenAI streaming tool_calls deltas back
// into a per-index accumulator so the assertions below can ignore which
// chunk carried which fragment.
type streamToolCallAcc struct {
	id   string
	name string
	args strings.Builder
}

func collectStreamToolCalls(t *testing.T, stream llmtypes.Stream) (map[int]*streamToolCallAcc, string) {
	t.Helper()
	acc := make(map[int]*streamToolCallAcc)
	finish := ""
	for {
		event, err := stream.Recv()
		if errors.Is(err, llmtypes.ErrStreamDone) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if len(event.Choices) == 0 {
			continue
		}
		choice := event.Choices[0]
		if choice.FinishReason != "" {
			finish = choice.FinishReason
		}
		raw, ok := choice.Delta.Extra["tool_calls"]
		if !ok {
			continue
		}
		var calls []map[string]any
		if err := json.Unmarshal(raw, &calls); err != nil {
			t.Fatalf("decode tool_calls delta: %v", err)
		}
		for _, c := range calls {
			idxFloat, _ := c["index"].(float64)
			idx := int(idxFloat)
			slot := acc[idx]
			if slot == nil {
				slot = &streamToolCallAcc{}
				acc[idx] = slot
			}
			if id, ok := c["id"].(string); ok && id != "" {
				slot.id = id
			}
			if function, ok := c["function"].(map[string]any); ok {
				if name, ok := function["name"].(string); ok && name != "" {
					slot.name = name
				}
				if args, ok := function["arguments"].(string); ok {
					slot.args.WriteString(args)
				}
			}
		}
	}
	return acc, finish
}

func TestCompleteStream_ToolUse_Standard(t *testing.T) {
	server := newAnthropicStreamServer(t, nil,
		messageStart("msg-1", "claude-x", 3),
		toolBlockStart(0, "call-1", "get_time", "{}"),
		inputJSONDelta(0, `{"tz":`),
		inputJSONDelta(0, `"UTC"}`),
		blockStop(0),
		messageDelta("tool_use", 5),
		messageStop(),
	)
	defer server.Close()
	stream := openAnthropicTestStream(t, server, "claude-x", "what time?")
	defer stream.Close()

	acc, finish := collectStreamToolCalls(t, stream)
	if finish != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", finish)
	}
	if len(acc) != 1 {
		t.Fatalf("tool calls len = %d, want 1", len(acc))
	}
	got := acc[0]
	if got.id != "call-1" {
		t.Errorf("id = %q, want call-1", got.id)
	}
	if got.name != "get_time" {
		t.Errorf("name = %q, want get_time", got.name)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got.args.String()), &parsed); err != nil {
		t.Fatalf("accumulated args %q invalid JSON: %v", got.args.String(), err)
	}
	if parsed["tz"] != "UTC" {
		t.Errorf("decoded args = %v, want {tz:UTC}", parsed)
	}
}

func TestCompleteStream_ToolUse_ZeroArgFlushedOnStop(t *testing.T) {
	server := newAnthropicStreamServer(t, nil,
		messageStart("msg-1", "claude-x", 2),
		toolBlockStart(0, "call-1", "noop", "{}"),
		blockStop(0),
		messageDelta("tool_use", 1),
		messageStop(),
	)
	defer server.Close()
	stream := openAnthropicTestStream(t, server, "claude-x", "go")
	defer stream.Close()

	acc, _ := collectStreamToolCalls(t, stream)
	if len(acc) != 1 {
		t.Fatalf("tool calls len = %d, want 1", len(acc))
	}
	got := acc[0]
	if got.id != "call-1" {
		t.Errorf("id = %q, want call-1", got.id)
	}
	if got.args.String() != "{}" {
		t.Errorf("args = %q, want {}", got.args.String())
	}
}

func TestCompleteStream_ToolUse_MultipleCalls(t *testing.T) {
	server := newAnthropicStreamServer(t, nil,
		messageStart("msg-1", "claude-x", 4),
		toolBlockStart(0, "a", "first", "{}"),
		inputJSONDelta(0, `{"k":1}`),
		blockStop(0),
		toolBlockStart(1, "b", "second", "{}"),
		inputJSONDelta(1, `{"k":2}`),
		blockStop(1),
		messageDelta("tool_use", 3),
		messageStop(),
	)
	defer server.Close()
	stream := openAnthropicTestStream(t, server, "claude-x", "do both")
	defer stream.Close()

	acc, _ := collectStreamToolCalls(t, stream)
	if len(acc) != 2 {
		t.Fatalf("tool calls len = %d, want 2", len(acc))
	}
	if acc[0].id != "a" || acc[0].name != "first" || acc[0].args.String() != `{"k":1}` {
		t.Errorf("call[0] = %+v", acc[0])
	}
	if acc[1].id != "b" || acc[1].name != "second" || acc[1].args.String() != `{"k":2}` {
		t.Errorf("call[1] = %+v", acc[1])
	}
}

func TestCompleteStream_ToolUse_MixedTextAndToolCall(t *testing.T) {
	server := newAnthropicStreamServer(t, nil,
		messageStart("msg-1", "claude-x", 2),
		textBlockStart(0),
		textDelta(0, "let me check"),
		blockStop(0),
		toolBlockStart(1, "call-1", "lookup", "{}"),
		inputJSONDelta(1, `{"q":"x"}`),
		blockStop(1),
		messageDelta("tool_use", 4),
		messageStop(),
	)
	defer server.Close()
	stream := openAnthropicTestStream(t, server, "claude-x", "go")
	defer stream.Close()

	var content strings.Builder
	acc := make(map[int]*streamToolCallAcc)
	for {
		event, err := stream.Recv()
		if errors.Is(err, llmtypes.ErrStreamDone) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if len(event.Choices) == 0 {
			continue
		}
		choice := event.Choices[0]
		content.WriteString(choice.Delta.Content)
		if raw, ok := choice.Delta.Extra["tool_calls"]; ok {
			var calls []map[string]any
			if err := json.Unmarshal(raw, &calls); err != nil {
				t.Fatalf("decode tool_calls: %v", err)
			}
			for _, c := range calls {
				idxFloat, _ := c["index"].(float64)
				idx := int(idxFloat)
				slot := acc[idx]
				if slot == nil {
					slot = &streamToolCallAcc{}
					acc[idx] = slot
				}
				if id, ok := c["id"].(string); ok && id != "" {
					slot.id = id
				}
				if function, ok := c["function"].(map[string]any); ok {
					if name, ok := function["name"].(string); ok && name != "" {
						slot.name = name
					}
					if args, ok := function["arguments"].(string); ok {
						slot.args.WriteString(args)
					}
				}
			}
		}
	}
	if content.String() != "let me check" {
		t.Errorf("content = %q, want \"let me check\"", content.String())
	}
	if len(acc) != 1 || acc[0].id != "call-1" {
		t.Errorf("tool calls = %v, want a single call call-1", acc)
	}
}
