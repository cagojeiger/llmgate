package anthropic

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"llmgate/internal/domain/llmtypes"
)

func TestCompleteStream_Success(t *testing.T) {
	server := newAnthropicStreamServer(t, func(t *testing.T, r *http.Request) {
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
	},
		messageStart("msg-1", "minimax-m2.5", 3),
		pingEvent(),
		textBlockStart(0),
		thinkingDelta(0, "because"),
		textDelta(0, "hel"),
		textDelta(0, "lo"),
		messageDeltaWithCache("end_turn", 2, 1, 2),
		messageStop(),
	)
	defer server.Close()
	stream := openAnthropicTestStream(t, server, "minimax-m2.5", "ping")
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
	server := newAnthropicStreamServer(t, nil,
		messageStart("msg-1", "minimax-m2.5", 1),
		streamError("overloaded_error", "stream exploded"),
	)
	defer server.Close()
	stream := openAnthropicTestStream(t, server, "minimax-m2.5", "ping")
	defer stream.Close()

	if _, err := stream.Recv(); err != nil {
		t.Fatalf("first Recv() error = %v", err)
	}
	_, err := stream.Recv()
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
	server := newAnthropicStreamServer(t, nil,
		messageStart("msg-1", "minimax-m2.5", 1),
		textDelta(0, "a"),
	)
	defer server.Close()
	stream := openAnthropicTestStream(t, server, "minimax-m2.5", "ping")
	defer stream.Close()

	if _, err := stream.Recv(); err != nil {
		t.Fatalf("first Recv() error = %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("second Recv() error = %v", err)
	}
	_, err := stream.Recv()
	perr := requireProviderError(t, err)
	if perr.Kind != llmtypes.KindUpstream {
		t.Fatalf("Kind = %q, want %q", perr.Kind, llmtypes.KindUpstream)
	}
	if !strings.Contains(perr.Message, "stream ended without message_stop") {
		t.Fatalf("Message = %q, want missing message_stop", perr.Message)
	}
}

func TestCompleteStream_PingIgnored(t *testing.T) {
	server := newAnthropicStreamServer(t, nil,
		pingEvent(),
		messageStart("msg-1", "minimax-m2.5", 1),
		messageDelta("end_turn", 1),
		messageStop(),
	)
	defer server.Close()
	stream := openAnthropicTestStream(t, server, "minimax-m2.5", "ping")
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
	server := newAnthropicStreamServer(t, nil,
		messageStart("msg-1", "minimax-m2.5", 3),
		textDelta(0, "hi"),
		messageDelta("end_turn", 2),
		messageStop(),
	)
	defer server.Close()
	stream := openAnthropicTestStream(t, server, "minimax-m2.5", "ping")
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
	server := newAnthropicStreamServer(t, nil,
		messageStart("msg-1", "minimax-m2.5", 7),
		textDelta(0, "a"),
	)
	defer server.Close()
	stream := openAnthropicTestStream(t, server, "minimax-m2.5", "ping")
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
