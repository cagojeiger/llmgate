package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"llmgate/internal/llmtypes"
)

func TestCompleteStream_Success(t *testing.T) {
	server := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream", got)
		}
		body, _ := io.ReadAll(r.Body)
		var raw map[string]any
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}
		if raw["stream"] != true {
			t.Fatalf("stream = %v, want true", raw["stream"])
		}

		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEEvent(t, w, "message_start", `
		{
			"type": "message_start",
			"message": {
				"id": "msg-1",
				"type": "message",
				"role": "assistant",
				"model": "minimax-m2.5",
				"usage": {
					"input_tokens": 3
				}
			}
		}
		`)
		writeSSEEvent(t, w, "ping", `{"type":"ping"}`)
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
				"type": "thinking_delta",
				"thinking": "because"
			}
		}
		`)
		writeSSEEvent(t, w, "content_block_delta", `
		{
			"type": "content_block_delta",
			"index": 0,
			"delta": {
				"type": "text_delta",
				"text": "hel"
			}
		}
		`)
		writeSSEEvent(t, w, "content_block_delta", `
		{
			"type": "content_block_delta",
			"index": 0,
			"delta": {
				"type": "text_delta",
				"text": "lo"
			}
		}
		`)
		writeSSEEvent(t, w, "message_delta", `
		{
			"type": "message_delta",
			"delta": {
				"stop_reason": "end_turn",
				"stop_sequence": null
			},
			"usage": {
				"output_tokens": 2,
				"cache_creation_input_tokens": 1,
				"cache_read_input_tokens": 2
			}
		}
		`)
		writeSSEEvent(t, w, "message_stop", `{"type":"message_stop"}`)
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "test-key", HTTPClient: server.Client, Name: "opencode"})
	stream, err := c.CompleteStream(context.Background(), &llmtypes.Request{
		Model:    "minimax-m2.5",
		Messages: []llmtypes.Message{{Role: "user", Content: "ping"}},
	})
	if err != nil {
		t.Fatalf("CompleteStream returned error: %v", err)
	}
	defer stream.Close()

	var content strings.Builder
	var reasoning strings.Builder
	var finishReason string
	var usage *llmtypes.Usage
	roleSeen := false
	chunks := 0
	for {
		event, err := stream.Recv()
		if errors.Is(err, llmtypes.ErrStreamDone) {
			break
		}
		if err != nil {
			t.Fatalf("Recv() error = %v", err)
		}
		chunks++
		if len(event.Choices) == 0 {
			continue
		}
		choice := event.Choices[0]
		if choice.Delta.Role == "assistant" {
			roleSeen = true
		}
		content.WriteString(choice.Delta.Content)
		reasoning.WriteString(choice.Delta.ReasoningContent)
		if choice.FinishReason != "" {
			finishReason = choice.FinishReason
			usage = event.Usage
		}
	}

	if chunks != 5 {
		t.Fatalf("chunks = %d, want 5", chunks)
	}
	if !roleSeen {
		t.Fatalf("assistant role chunk missing")
	}
	if content.String() != "hello" {
		t.Fatalf("content = %q, want hello", content.String())
	}
	if reasoning.String() != "because" {
		t.Fatalf("reasoning = %q, want because", reasoning.String())
	}
	if finishReason != "stop" {
		t.Fatalf("finishReason = %q, want stop", finishReason)
	}
	if usage == nil || usage.TotalTokens != 5 {
		t.Fatalf("usage = %+v, want total 5", usage)
	}
	if string(usage.Extra["cache_creation_input_tokens"]) != "1" {
		t.Fatalf("cache_creation_input_tokens = %s, want 1", usage.Extra["cache_creation_input_tokens"])
	}
	if string(usage.Extra["cache_read_input_tokens"]) != "2" {
		t.Fatalf("cache_read_input_tokens = %s, want 2", usage.Extra["cache_read_input_tokens"])
	}
}

func TestCompleteStream_ErrorMidFlight(t *testing.T) {
	server := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEEvent(t, w, "message_start", `
		{
			"type": "message_start",
			"message": {
				"id": "msg-1",
				"type": "message",
				"role": "assistant",
				"model": "minimax-m2.5",
				"usage": {
					"input_tokens": 1
				}
			}
		}
		`)
		writeSSEEvent(t, w, "error", `{"type":"error","error":{"type":"overloaded_error","message":"stream exploded"}}`)
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "test-key", HTTPClient: server.Client, Name: "opencode"})
	stream, err := c.CompleteStream(context.Background(), &llmtypes.Request{
		Model:    "minimax-m2.5",
		Messages: []llmtypes.Message{{Role: "user", Content: "ping"}},
	})
	if err != nil {
		t.Fatalf("CompleteStream returned error: %v", err)
	}
	defer stream.Close()

	if _, err := stream.Recv(); err != nil {
		t.Fatalf("first Recv() error = %v", err)
	}
	_, err = stream.Recv()
	perr := requireProviderError(t, err)
	if perr.Kind != llmtypes.KindUpstream {
		t.Fatalf("Kind = %q, want %q", perr.Kind, llmtypes.KindUpstream)
	}
	if perr.Message != "upstream unavailable" {
		t.Fatalf("Message = %q, want sanitized upstream message", perr.Message)
	}
	if !strings.Contains(string(perr.Raw), "stream exploded") {
		t.Fatalf("Raw = %q, want original stream error preserved", string(perr.Raw))
	}
}

func TestCompleteStream_NoMessageStop(t *testing.T) {
	server := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEEvent(t, w, "message_start", `
		{
			"type": "message_start",
			"message": {
				"id": "msg-1",
				"type": "message",
				"role": "assistant",
				"model": "minimax-m2.5",
				"usage": {
					"input_tokens": 1
				}
			}
		}
		`)
		writeSSEEvent(t, w, "content_block_delta", `
		{
			"type": "content_block_delta",
			"index": 0,
			"delta": {
				"type": "text_delta",
				"text": "a"
			}
		}
		`)
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "test-key", HTTPClient: server.Client, Name: "opencode"})
	stream, err := c.CompleteStream(context.Background(), &llmtypes.Request{
		Model:    "minimax-m2.5",
		Messages: []llmtypes.Message{{Role: "user", Content: "ping"}},
	})
	if err != nil {
		t.Fatalf("CompleteStream returned error: %v", err)
	}
	defer stream.Close()

	if _, err := stream.Recv(); err != nil {
		t.Fatalf("first Recv() error = %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("second Recv() error = %v", err)
	}
	_, err = stream.Recv()
	perr := requireProviderError(t, err)
	if perr.Kind != llmtypes.KindUpstream {
		t.Fatalf("Kind = %q, want %q", perr.Kind, llmtypes.KindUpstream)
	}
	if !strings.Contains(perr.Message, "stream ended without message_stop") {
		t.Fatalf("Message = %q, want missing message_stop", perr.Message)
	}
}

func TestCompleteStream_PingIgnored(t *testing.T) {
	server := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEEvent(t, w, "ping", `{"type":"ping"}`)
		writeSSEEvent(t, w, "message_start", `
		{
			"type": "message_start",
			"message": {
				"id": "msg-1",
				"type": "message",
				"role": "assistant",
				"model": "minimax-m2.5",
				"usage": {
					"input_tokens": 1
				}
			}
		}
		`)
		writeSSEEvent(t, w, "message_delta", `
		{
			"type": "message_delta",
			"delta": {
				"stop_reason": "end_turn",
				"stop_sequence": null
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
		Model:    "minimax-m2.5",
		Messages: []llmtypes.Message{{Role: "user", Content: "ping"}},
	})
	if err != nil {
		t.Fatalf("CompleteStream returned error: %v", err)
	}
	defer stream.Close()

	event, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv() error = %v", err)
	}
	if len(event.Choices) != 1 || event.Choices[0].Delta.Role != "assistant" {
		t.Fatalf("event = %+v, want assistant role after ping", event)
	}
}

func TestStreamSummary_Success(t *testing.T) {
	server := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEEvent(t, w, "message_start", `
		{
			"type": "message_start",
			"message": {
				"id": "msg-1",
				"type": "message",
				"role": "assistant",
				"model": "minimax-m2.5",
				"usage": {
					"input_tokens": 3
				}
			}
		}
		`)
		writeSSEEvent(t, w, "content_block_delta", `
		{
			"type": "content_block_delta",
			"index": 0,
			"delta": {
				"type": "text_delta",
				"text": "hi"
			}
		}
		`)
		writeSSEEvent(t, w, "message_delta", `
		{
			"type": "message_delta",
			"delta": {
				"stop_reason": "end_turn",
				"stop_sequence": null
			},
			"usage": {
				"output_tokens": 2
			}
		}
		`)
		writeSSEEvent(t, w, "message_stop", `{"type":"message_stop"}`)
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "test-key", HTTPClient: server.Client, Name: "opencode"})
	stream, err := c.CompleteStream(context.Background(), &llmtypes.Request{
		Model:    "minimax-m2.5",
		Messages: []llmtypes.Message{{Role: "user", Content: "ping"}},
	})
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	defer stream.Close()

	for {
		if _, err := stream.Recv(); errors.Is(err, llmtypes.ErrStreamDone) {
			break
		} else if err != nil {
			t.Fatalf("Recv: %v", err)
		}
	}

	sum := stream.Summary()
	if sum == nil {
		t.Fatal("Summary returned nil")
	}
	if sum.Model != "minimax-m2.5" {
		t.Errorf("Model = %q, want minimax-m2.5", sum.Model)
	}
	if sum.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop (mapped from end_turn)", sum.FinishReason)
	}
	if sum.Usage == nil || sum.Usage.PromptTokens != 3 || sum.Usage.CompletionTokens != 2 || sum.Usage.TotalTokens != 5 {
		t.Errorf("Usage = %+v, want {3,2,5}", sum.Usage)
	}
	if sum.ChunkCount < 3 {
		t.Errorf("ChunkCount = %d, want >= 3", sum.ChunkCount)
	}
	if sum.FirstByteAt.IsZero() {
		t.Error("FirstByteAt is zero, want set")
	}
}

func TestStreamSummary_PartialOnError(t *testing.T) {
	// message_start arrives (prompt tokens known) but stream cuts off before
	// message_delta. Summary should expose prompt-side tokens for audit even
	// though completion never finished.
	server := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEEvent(t, w, "message_start", `
		{
			"type": "message_start",
			"message": {
				"id": "msg-1",
				"type": "message",
				"role": "assistant",
				"model": "minimax-m2.5",
				"usage": {
					"input_tokens": 7
				}
			}
		}
		`)
		writeSSEEvent(t, w, "content_block_delta", `
		{
			"type": "content_block_delta",
			"index": 0,
			"delta": {
				"type": "text_delta",
				"text": "a"
			}
		}
		`)
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "test-key", HTTPClient: server.Client, Name: "opencode"})
	stream, err := c.CompleteStream(context.Background(), &llmtypes.Request{
		Model:    "minimax-m2.5",
		Messages: []llmtypes.Message{{Role: "user", Content: "ping"}},
	})
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	defer stream.Close()

	if _, err := stream.Recv(); err != nil {
		t.Fatalf("first Recv: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("second Recv: %v", err)
	}
	if _, err := stream.Recv(); err == nil {
		t.Fatal("third Recv: want error (truncated stream)")
	}

	sum := stream.Summary()
	if sum.Usage == nil || sum.Usage.PromptTokens != 7 {
		t.Errorf("Usage = %+v, want PromptTokens=7", sum.Usage)
	}
	if sum.Usage.CompletionTokens != 0 {
		t.Errorf("CompletionTokens = %d, want 0 (never finished)", sum.Usage.CompletionTokens)
	}
	if sum.FinishReason != "" {
		t.Errorf("FinishReason = %q, want empty (no message_delta)", sum.FinishReason)
	}
	if sum.FirstByteAt.IsZero() {
		t.Error("FirstByteAt zero, want set after first emitted event")
	}
}
