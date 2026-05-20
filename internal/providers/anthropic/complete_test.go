package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"llmgate/internal/domain/llmtypes"
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
	resp, err := c.Complete(context.Background(), &llmtypes.Request{
		Model:     "minimax-m2.5",
		Messages:  []llmtypes.Message{{Role: "user", Content: "ping"}},
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
	_, err := c.Complete(context.Background(), &llmtypes.Request{
		Model: "minimax-m2.5",
		Messages: []llmtypes.Message{
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
	resp, err := c.Complete(context.Background(), &llmtypes.Request{
		Model:    "minimax-m2.5",
		Messages: []llmtypes.Message{{Role: "user", Content: "ping"}},
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

func TestComplete_InvalidSuccessBodySanitizesDecodeDetail(t *testing.T) {
	server := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html>internal stack from upstream</html>`))
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "k", HTTPClient: server.Client, Name: "opencode"})
	_, err := c.Complete(context.Background(), &llmtypes.Request{
		Model:    "minimax-m2.5",
		Messages: []llmtypes.Message{{Role: "user", Content: "ping"}},
	})
	perr := requireProviderError(t, err)
	if perr.Kind != llmtypes.KindUpstream {
		t.Fatalf("Kind = %q, want %q", perr.Kind, llmtypes.KindUpstream)
	}
	if perr.Message != "upstream returned invalid response" {
		t.Fatalf("Message = %q, want sanitized invalid response", perr.Message)
	}
	if strings.Contains(perr.Message, "<html>") || strings.Contains(perr.Message, "invalid character") {
		t.Fatalf("Message leaked parser/body detail: %q", perr.Message)
	}
	if !strings.Contains(string(perr.Raw), "internal stack") {
		t.Fatalf("Raw = %q, want original invalid body preserved", string(perr.Raw))
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
	_, err := c.Complete(context.Background(), &llmtypes.Request{
		Model:    "minimax-m2.5",
		Messages: []llmtypes.Message{{Role: "user", Content: "ping"}},
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
}

// TestClassify_ContentFilterOverridesStatus ensures the envelope's
// content_filter signal beats the broader HTTP status code (400/422
// would otherwise lock us into KindBadRequest), matching the OpenAI
// adapter's behavior.

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
			resp, err := c.Complete(context.Background(), &llmtypes.Request{
				Model:    "minimax-m2.5",
				Messages: []llmtypes.Message{{Role: "user", Content: "ping"}},
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
