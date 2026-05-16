package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"llmgate/internal/llmtypes"
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
	server := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEEvent(t, w, "message_start", `
		{
			"type": "message_start",
			"message": {
				"id": "msg-1",
				"type": "message",
				"role": "assistant",
				"model": "claude-x",
				"usage": {
					"input_tokens": 3
				}
			}
		}
		`)
		writeSSEEvent(t, w, "content_block_start", `
		{
			"type": "content_block_start",
			"index": 0,
			"content_block": {
				"type": "tool_use",
				"id": "call-1",
				"name": "get_time",
				"input": {}
			}
		}
		`)
		writeSSEEvent(t, w, "content_block_delta", `
		{
			"type": "content_block_delta",
			"index": 0,
			"delta": {
				"type": "input_json_delta",
				"partial_json": "{\"tz\":"
			}
		}
		`)
		writeSSEEvent(t, w, "content_block_delta", `
		{
			"type": "content_block_delta",
			"index": 0,
			"delta": {
				"type": "input_json_delta",
				"partial_json": "\"UTC\"}"
			}
		}
		`)
		writeSSEEvent(t, w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
		writeSSEEvent(t, w, "message_delta", `
		{
			"type": "message_delta",
			"delta": {
				"stop_reason": "tool_use",
				"stop_sequence": null
			},
			"usage": {
				"output_tokens": 5
			}
		}
		`)
		writeSSEEvent(t, w, "message_stop", `{"type":"message_stop"}`)
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "test-key", HTTPClient: server.Client, Name: "opencode"})
	stream, err := c.CompleteStream(context.Background(), &llmtypes.Request{
		Model:    "claude-x",
		Messages: []llmtypes.Message{{Role: "user", Content: "what time?"}},
	})
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
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
	server := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEEvent(t, w, "message_start", `
		{
			"type": "message_start",
			"message": {
				"id": "msg-1",
				"type": "message",
				"role": "assistant",
				"model": "claude-x",
				"usage": {
					"input_tokens": 2
				}
			}
		}
		`)
		writeSSEEvent(t, w, "content_block_start", `
		{
			"type": "content_block_start",
			"index": 0,
			"content_block": {
				"type": "tool_use",
				"id": "call-1",
				"name": "noop",
				"input": {}
			}
		}
		`)
		// no input_json_delta: zero-argument tool
		writeSSEEvent(t, w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
		writeSSEEvent(t, w, "message_delta", `
		{
			"type": "message_delta",
			"delta": {
				"stop_reason": "tool_use"
			},
			"usage": {
				"output_tokens": 1
			}
		}
		`)
		writeSSEEvent(t, w, "message_stop", `{"type":"message_stop"}`)
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "test-key", HTTPClient: server.Client, Name: "opencode"})
	stream, err := c.CompleteStream(context.Background(), &llmtypes.Request{
		Model:    "claude-x",
		Messages: []llmtypes.Message{{Role: "user", Content: "go"}},
	})
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
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
	server := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEEvent(t, w, "message_start", `
		{
			"type": "message_start",
			"message": {
				"id": "msg-1",
				"type": "message",
				"role": "assistant",
				"model": "claude-x",
				"usage": {
					"input_tokens": 4
				}
			}
		}
		`)
		writeSSEEvent(t, w, "content_block_start", `
		{
			"type": "content_block_start",
			"index": 0,
			"content_block": {
				"type": "tool_use",
				"id": "a",
				"name": "first",
				"input": {}
			}
		}
		`)
		writeSSEEvent(t, w, "content_block_delta", `
		{
			"type": "content_block_delta",
			"index": 0,
			"delta": {
				"type": "input_json_delta",
				"partial_json": "{\"k\":1}"
			}
		}
		`)
		writeSSEEvent(t, w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
		writeSSEEvent(t, w, "content_block_start", `
		{
			"type": "content_block_start",
			"index": 1,
			"content_block": {
				"type": "tool_use",
				"id": "b",
				"name": "second",
				"input": {}
			}
		}
		`)
		writeSSEEvent(t, w, "content_block_delta", `
		{
			"type": "content_block_delta",
			"index": 1,
			"delta": {
				"type": "input_json_delta",
				"partial_json": "{\"k\":2}"
			}
		}
		`)
		writeSSEEvent(t, w, "content_block_stop", `{"type":"content_block_stop","index":1}`)
		writeSSEEvent(t, w, "message_delta", `
		{
			"type": "message_delta",
			"delta": {
				"stop_reason": "tool_use"
			},
			"usage": {
				"output_tokens": 3
			}
		}
		`)
		writeSSEEvent(t, w, "message_stop", `{"type":"message_stop"}`)
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "test-key", HTTPClient: server.Client, Name: "opencode"})
	stream, err := c.CompleteStream(context.Background(), &llmtypes.Request{
		Model:    "claude-x",
		Messages: []llmtypes.Message{{Role: "user", Content: "do both"}},
	})
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
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
	server := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEEvent(t, w, "message_start", `
		{
			"type": "message_start",
			"message": {
				"id": "msg-1",
				"type": "message",
				"role": "assistant",
				"model": "claude-x",
				"usage": {
					"input_tokens": 2
				}
			}
		}
		`)
		writeSSEEvent(t, w, "content_block_start", `
		{
			"type": "content_block_start",
			"index": 0,
			"content_block": {
				"type": "text",
				"text": ""
			}
		}
		`)
		writeSSEEvent(t, w, "content_block_delta", `
		{
			"type": "content_block_delta",
			"index": 0,
			"delta": {
				"type": "text_delta",
				"text": "let me check"
			}
		}
		`)
		writeSSEEvent(t, w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
		writeSSEEvent(t, w, "content_block_start", `
		{
			"type": "content_block_start",
			"index": 1,
			"content_block": {
				"type": "tool_use",
				"id": "call-1",
				"name": "lookup",
				"input": {}
			}
		}
		`)
		writeSSEEvent(t, w, "content_block_delta", `
		{
			"type": "content_block_delta",
			"index": 1,
			"delta": {
				"type": "input_json_delta",
				"partial_json": "{\"q\":\"x\"}"
			}
		}
		`)
		writeSSEEvent(t, w, "content_block_stop", `{"type":"content_block_stop","index":1}`)
		writeSSEEvent(t, w, "message_delta", `
		{
			"type": "message_delta",
			"delta": {
				"stop_reason": "tool_use"
			},
			"usage": {
				"output_tokens": 4
			}
		}
		`)
		writeSSEEvent(t, w, "message_stop", `{"type":"message_stop"}`)
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "test-key", HTTPClient: server.Client, Name: "opencode"})
	stream, err := c.CompleteStream(context.Background(), &llmtypes.Request{
		Model:    "claude-x",
		Messages: []llmtypes.Message{{Role: "user", Content: "go"}},
	})
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
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
