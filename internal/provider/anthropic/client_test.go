package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"llmgate/internal/provider"
)

func TestComplete_Success(t *testing.T) {
	server := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/messages" {
			t.Errorf("path = %s, want /messages", r.URL.Path)
		}
		if got := r.Header.Get("X-Api-Key"); got != "test-key" {
			t.Errorf("X-Api-Key = %q, want test-key", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q, want application/json", got)
		}
		if got := r.Header.Get("User-Agent"); got != defaultUserAgent {
			t.Errorf("User-Agent = %q, want %q", got, defaultUserAgent)
		}

		body, _ := io.ReadAll(r.Body)
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}
		if got["model"] != "minimax-m2.5" {
			t.Errorf("model = %q, want minimax-m2.5", got["model"])
		}
		if got["vendor_request"] != "keep" {
			t.Errorf("vendor_request = %v, want keep", got["vendor_request"])
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "msg-1",
			"type": "message",
			"role": "assistant",
			"model": "minimax-m2.5",
			"content": [{"type": "text", "text": "pong"}],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 5, "output_tokens": 1}
		}`))
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "test-key", HTTPClient: server.Client, Name: "opencode"})
	resp, err := c.Complete(context.Background(), &provider.Request{
		Model:     "minimax-m2.5",
		Messages:  []provider.Message{{Role: "user", Content: "ping"}},
		MaxTokens: 32,
		Extra:     map[string]json.RawMessage{"vendor_request": json.RawMessage(`"keep"`)},
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	if resp.ID != "msg-1" {
		t.Errorf("ID = %q, want msg-1", resp.ID)
	}
	if resp.Object != "chat.completion" {
		t.Errorf("Object = %q, want chat.completion", resp.Object)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("len(Choices) = %d, want 1", len(resp.Choices))
	}
	if resp.Choices[0].Message.Content != "pong" {
		t.Errorf("content = %q, want pong", resp.Choices[0].Message.Content)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", resp.Choices[0].FinishReason)
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 6 {
		t.Errorf("usage = %+v, want TotalTokens=6", resp.Usage)
	}
}

func TestComplete_SystemMessageExtracted(t *testing.T) {
	server := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var got struct {
			System   string `json:"system"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}
		if got.System != "policy one\n\npolicy two" {
			t.Errorf("system = %q, want joined system messages", got.System)
		}
		if len(got.Messages) != 1 {
			t.Fatalf("len(messages) = %d, want 1", len(got.Messages))
		}
		if got.Messages[0].Role != "user" || string(got.Messages[0].Content) != `"ping"` {
			t.Errorf("message = %+v, want user ping", got.Messages[0])
		}
		writeAnthropicResponse(t, w, "end_turn")
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "test-key", HTTPClient: server.Client})
	_, err := c.Complete(context.Background(), &provider.Request{
		Model: "minimax-m2.5",
		Messages: []provider.Message{
			{Role: "system", Content: "policy one"},
			{Role: "user", Content: "ping"},
			{Role: "system", Content: "policy two"},
		},
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
}

func TestComplete_ThinkingContentMappedToReasoning(t *testing.T) {
	server := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "msg-1",
			"type": "message",
			"role": "assistant",
			"model": "minimax-m2.5",
			"content": [
				{"type": "thinking", "thinking": "because"},
				{"type": "text", "text": "pong"}
			],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 5, "output_tokens": 2}
		}`))
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "test-key", HTTPClient: server.Client})
	resp, err := c.Complete(context.Background(), &provider.Request{
		Model:    "minimax-m2.5",
		Messages: []provider.Message{{Role: "user", Content: "ping"}},
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	msg := resp.Choices[0].Message
	if msg.Content != "pong" {
		t.Fatalf("content = %q, want pong", msg.Content)
	}
	if msg.ReasoningContent != "because" {
		t.Fatalf("reasoning_content = %q, want because", msg.ReasoningContent)
	}
}

func TestComplete_MaxTokensDefault(t *testing.T) {
	server := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var got struct {
			MaxTokens int `json:"max_tokens"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}
		if got.MaxTokens != 4096 {
			t.Errorf("max_tokens = %d, want 4096", got.MaxTokens)
		}
		writeAnthropicResponse(t, w, "end_turn")
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "test-key", HTTPClient: server.Client})
	_, err := c.Complete(context.Background(), &provider.Request{
		Model:    "minimax-m2.5",
		Messages: []provider.Message{{Role: "user", Content: "ping"}},
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
}

// TestClassify_ContentFilterOverridesStatus ensures the envelope's
// content_filter signal beats the broader HTTP status code (400/422
// would otherwise lock us into KindBadRequest), matching the OpenAI
// adapter's behavior.
func TestClassify_ContentFilterOverridesStatus(t *testing.T) {
	c := mustNew(t, Config{BaseURL: "http://example.invalid", APIKey: "k", Name: "opencode"})
	cases := []struct {
		name   string
		status int
		body   string
		want   provider.Kind
	}{
		{
			name:   "400 + content_filter",
			status: 400,
			body:   `{"type":"error","error":{"type":"content_filter","message":"blocked by policy"}}`,
			want:   provider.KindContentFilter,
		},
		{
			name:   "422 + content_filter_error",
			status: 422,
			body:   `{"type":"error","error":{"type":"content_filter_error","message":"blocked"}}`,
			want:   provider.KindContentFilter,
		},
		{
			name:   "400 + invalid_request_error stays bad_request",
			status: 400,
			body:   `{"type":"error","error":{"type":"invalid_request_error","message":"bad field"}}`,
			want:   provider.KindBadRequest,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			perr := c.classify(tc.status, []byte(tc.body), "")
			if perr.Kind != tc.want {
				t.Errorf("Kind = %q, want %q", perr.Kind, tc.want)
			}
			if perr.StatusCode != tc.status {
				t.Errorf("StatusCode = %d, want %d", perr.StatusCode, tc.status)
			}
		})
	}
}

// TestKindFromAnthropicErrorType pins the envelope error.type → Kind
// mapping. Update this table when Anthropic ships new error types.
func TestKindFromAnthropicErrorType(t *testing.T) {
	cases := []struct {
		errorType string
		want      provider.Kind
	}{
		{"authentication_error", provider.KindAuth},
		{"permission_error", provider.KindAuth},
		{"invalid_request_error", provider.KindBadRequest},
		{"not_found_error", provider.KindBadRequest},
		{"request_too_large", provider.KindBadRequest},
		{"rate_limit_error", provider.KindRateLimit},
		{"content_filter", provider.KindContentFilter},
		{"content_filter_error", provider.KindContentFilter},
		{"overloaded_error", provider.KindUpstream},
		{"api_error", provider.KindUpstream},
		{"future_unknown_2030", provider.KindUpstream},
	}
	for _, tc := range cases {
		t.Run(tc.errorType, func(t *testing.T) {
			if got := kindFromAnthropicErrorType(tc.errorType); got != tc.want {
				t.Errorf("Kind = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestComplete_StopReasonMapping(t *testing.T) {
	cases := []struct {
		stopReason string
		want       string
	}{
		{"end_turn", "stop"},
		{"stop_sequence", "stop"},
		{"max_tokens", "length"},
		{"tool_use", "tool_calls"},
		{"refusal", "content_filter"},
		{"pause_turn", "stop"},
		{"future_unknown_2030", "stop"},
	}
	for _, tc := range cases {
		t.Run(tc.stopReason, func(t *testing.T) {
			server := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeAnthropicResponse(t, w, tc.stopReason)
			}))
			defer server.Close()

			c := mustNew(t, Config{BaseURL: server.URL, APIKey: "test-key", HTTPClient: server.Client})
			resp, err := c.Complete(context.Background(), &provider.Request{
				Model:    "minimax-m2.5",
				Messages: []provider.Message{{Role: "user", Content: "ping"}},
			})
			if err != nil {
				t.Fatalf("Complete returned error: %v", err)
			}
			if got := resp.Choices[0].FinishReason; got != tc.want {
				t.Fatalf("finish_reason = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestComplete_ErrorEnvelope(t *testing.T) {
	server := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid api key"}}`))
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "bad-key", HTTPClient: server.Client, Name: "opencode"})
	_, err := c.Complete(context.Background(), &provider.Request{
		Model:    "minimax-m2.5",
		Messages: []provider.Message{{Role: "user", Content: "ping"}},
	})
	perr := requireProviderError(t, err)
	if perr.Kind != provider.KindAuth {
		t.Errorf("Kind = %q, want %q", perr.Kind, provider.KindAuth)
	}
	if !strings.Contains(perr.Message, "invalid api key") {
		t.Errorf("Message = %q, want invalid api key", perr.Message)
	}
	if perr.Provider != "opencode" {
		t.Errorf("Provider = %q, want opencode", perr.Provider)
	}
}

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
		writeSSEEvent(t, w, "message_start", `{"type":"message_start","message":{"id":"msg-1","type":"message","role":"assistant","model":"minimax-m2.5","usage":{"input_tokens":3}}}`)
		writeSSEEvent(t, w, "ping", `{"type":"ping"}`)
		writeSSEEvent(t, w, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		writeSSEEvent(t, w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"because"}}`)
		writeSSEEvent(t, w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hel"}}`)
		writeSSEEvent(t, w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`)
		writeSSEEvent(t, w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2,"cache_creation_input_tokens":1,"cache_read_input_tokens":2}}`)
		writeSSEEvent(t, w, "message_stop", `{"type":"message_stop"}`)
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "test-key", HTTPClient: server.Client, Name: "opencode"})
	stream, err := c.CompleteStream(context.Background(), &provider.Request{
		Model:    "minimax-m2.5",
		Messages: []provider.Message{{Role: "user", Content: "ping"}},
	})
	if err != nil {
		t.Fatalf("CompleteStream returned error: %v", err)
	}
	defer stream.Close()

	var content strings.Builder
	var reasoning strings.Builder
	var finishReason string
	var usage *provider.Usage
	roleSeen := false
	chunks := 0
	for {
		event, err := stream.Recv()
		if errors.Is(err, provider.ErrStreamDone) {
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
		writeSSEEvent(t, w, "message_start", `{"type":"message_start","message":{"id":"msg-1","type":"message","role":"assistant","model":"minimax-m2.5","usage":{"input_tokens":1}}}`)
		writeSSEEvent(t, w, "error", `{"type":"error","error":{"type":"overloaded_error","message":"stream exploded"}}`)
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "test-key", HTTPClient: server.Client, Name: "opencode"})
	stream, err := c.CompleteStream(context.Background(), &provider.Request{
		Model:    "minimax-m2.5",
		Messages: []provider.Message{{Role: "user", Content: "ping"}},
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
	if perr.Kind != provider.KindUpstream {
		t.Fatalf("Kind = %q, want %q", perr.Kind, provider.KindUpstream)
	}
	if !strings.Contains(perr.Message, "stream exploded") {
		t.Fatalf("Message = %q, want stream exploded", perr.Message)
	}
}

func TestCompleteStream_NoMessageStop(t *testing.T) {
	server := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEEvent(t, w, "message_start", `{"type":"message_start","message":{"id":"msg-1","type":"message","role":"assistant","model":"minimax-m2.5","usage":{"input_tokens":1}}}`)
		writeSSEEvent(t, w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a"}}`)
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "test-key", HTTPClient: server.Client, Name: "opencode"})
	stream, err := c.CompleteStream(context.Background(), &provider.Request{
		Model:    "minimax-m2.5",
		Messages: []provider.Message{{Role: "user", Content: "ping"}},
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
	if perr.Kind != provider.KindUpstream {
		t.Fatalf("Kind = %q, want %q", perr.Kind, provider.KindUpstream)
	}
	if !strings.Contains(perr.Message, "stream ended without message_stop") {
		t.Fatalf("Message = %q, want missing message_stop", perr.Message)
	}
}

func TestCompleteStream_PingIgnored(t *testing.T) {
	server := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEEvent(t, w, "ping", `{"type":"ping"}`)
		writeSSEEvent(t, w, "message_start", `{"type":"message_start","message":{"id":"msg-1","type":"message","role":"assistant","model":"minimax-m2.5","usage":{"input_tokens":1}}}`)
		writeSSEEvent(t, w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`)
		writeSSEEvent(t, w, "message_stop", `{"type":"message_stop"}`)
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "test-key", HTTPClient: server.Client, Name: "opencode"})
	stream, err := c.CompleteStream(context.Background(), &provider.Request{
		Model:    "minimax-m2.5",
		Messages: []provider.Message{{Role: "user", Content: "ping"}},
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
		writeSSEEvent(t, w, "message_start", `{"type":"message_start","message":{"id":"msg-1","type":"message","role":"assistant","model":"minimax-m2.5","usage":{"input_tokens":3}}}`)
		writeSSEEvent(t, w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`)
		writeSSEEvent(t, w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}`)
		writeSSEEvent(t, w, "message_stop", `{"type":"message_stop"}`)
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "test-key", HTTPClient: server.Client, Name: "opencode"})
	stream, err := c.CompleteStream(context.Background(), &provider.Request{
		Model:    "minimax-m2.5",
		Messages: []provider.Message{{Role: "user", Content: "ping"}},
	})
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	defer stream.Close()

	for {
		if _, err := stream.Recv(); errors.Is(err, provider.ErrStreamDone) {
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
		writeSSEEvent(t, w, "message_start", `{"type":"message_start","message":{"id":"msg-1","type":"message","role":"assistant","model":"minimax-m2.5","usage":{"input_tokens":7}}}`)
		writeSSEEvent(t, w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a"}}`)
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "test-key", HTTPClient: server.Client, Name: "opencode"})
	stream, err := c.CompleteStream(context.Background(), &provider.Request{
		Model:    "minimax-m2.5",
		Messages: []provider.Message{{Role: "user", Content: "ping"}},
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

func writeAnthropicResponse(t *testing.T, w http.ResponseWriter, stopReason string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	_, err := w.Write([]byte(`{
		"id": "msg-1",
		"type": "message",
		"role": "assistant",
		"model": "minimax-m2.5",
		"content": [{"type": "text", "text": "pong"}],
		"stop_reason": "` + stopReason + `",
		"usage": {"input_tokens": 2, "output_tokens": 1}
	}`))
	if err != nil {
		t.Fatalf("write response: %v", err)
	}
}

func writeSSEEvent(t *testing.T, w http.ResponseWriter, event, payload string) {
	t.Helper()
	_, err := w.Write([]byte("event: " + event + "\n" + "data: " + payload + "\n\n"))
	if err != nil {
		t.Fatalf("write SSE event: %v", err)
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	time.Sleep(time.Millisecond)
}

func requireProviderError(t *testing.T, err error) *provider.Error {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var perr *provider.Error
	if !errors.As(err, &perr) {
		t.Fatalf("err type = %T, want *provider.Error", err)
	}
	return perr
}

func mustNew(t *testing.T, cfg Config) *Client {
	t.Helper()
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return c
}

type localServer struct {
	*httptest.Server
	Client *http.Client
}

func newLocalServer(handler http.Handler) *localServer {
	listener := newPipeListener()
	server := &httptest.Server{
		Listener: listener,
		Config:   &http.Server{Handler: handler},
	}
	server.Start()

	transport := &http.Transport{DialContext: listener.DialContext}
	return &localServer{
		Server: server,
		Client: &http.Client{Transport: transport},
	}
}

func (s *localServer) Close() {
	if transport, ok := s.Client.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
	s.Server.Close()
}

type pipeListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{
		conns:  make(chan net.Conn),
		closed: make(chan struct{}),
	}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() {
		close(l.closed)
	})
	return nil
}

func (l *pipeListener) Addr() net.Addr {
	return pipeAddr("pipe")
}

func (l *pipeListener) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	clientConn, serverConn := net.Pipe()
	select {
	case l.conns <- serverConn:
		return clientConn, nil
	case <-ctx.Done():
		_ = clientConn.Close()
		_ = serverConn.Close()
		return nil, ctx.Err()
	case <-l.closed:
		_ = clientConn.Close()
		_ = serverConn.Close()
		return nil, net.ErrClosed
	}
}

type pipeAddr string

func (a pipeAddr) Network() string { return string(a) }
func (a pipeAddr) String() string  { return string(a) }
