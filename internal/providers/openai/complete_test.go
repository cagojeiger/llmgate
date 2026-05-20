package openai

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
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %s, want /chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want Bearer test-key", got)
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
		var got llmtypes.Request
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}
		if got.Model != "deepseek-v4-flash" {
			t.Errorf("model = %q, want deepseek-v4-flash", got.Model)
		}
		if len(got.Messages) != 1 || got.Messages[0].Content != "ping" {
			t.Errorf("messages = %+v, want [{user,ping}]", got.Messages)
		}
		if string(got.Extra["vendor_request"]) != `"keep"` {
			t.Errorf("vendor_request extra = %s, want keep", got.Extra["vendor_request"])
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "chat-1",
			"object": "chat.completion",
			"model": "deepseek-v4-flash",
			"choices": [{
				"index": 0,
				"message": {"role": "assistant", "content": "pong", "reasoning_content": "because", "vendor_msg": 1},
				"finish_reason": "stop"
			}],
			"usage": {"prompt_tokens": 5, "completion_tokens": 1, "total_tokens": 6, "prompt_cache_hit_tokens": 4},
			"cost": 0.001
		}`))
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "test-key", HTTPClient: server.Client, Name: "opencode"})
	resp, err := c.Complete(context.Background(), &llmtypes.Request{
		Model:     "deepseek-v4-flash",
		Messages:  []llmtypes.Message{{Role: "user", Content: "ping"}},
		MaxTokens: 32,
		Extra:     map[string]json.RawMessage{"vendor_request": json.RawMessage(`"keep"`)},
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	if resp.ID != "chat-1" {
		t.Errorf("ID = %q, want chat-1", resp.ID)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("len(Choices) = %d, want 1", len(resp.Choices))
	}
	if resp.Choices[0].Message.Content != "pong" {
		t.Errorf("content = %q, want pong", resp.Choices[0].Message.Content)
	}
	if resp.Choices[0].Message.ReasoningContent != "because" {
		t.Errorf("reasoning_content = %q, want because", resp.Choices[0].Message.ReasoningContent)
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 6 {
		t.Errorf("usage = %+v, want TotalTokens=6", resp.Usage)
	}
	if string(resp.Extra["cost"]) != "0.001" {
		t.Errorf("cost extra = %s, want 0.001", resp.Extra["cost"])
	}
	if string(resp.Usage.Extra["prompt_cache_hit_tokens"]) != "4" {
		t.Errorf("usage extra = %s, want 4", resp.Usage.Extra["prompt_cache_hit_tokens"])
	}
}

func TestComplete_UpstreamErrorEnvelope(t *testing.T) {
	server := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key","type":"authentication_error"}}`))
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "bad-key", HTTPClient: server.Client, Name: "opencode"})
	_, err := c.Complete(context.Background(), &llmtypes.Request{
		Model:    "deepseek-v4-flash",
		Messages: []llmtypes.Message{{Role: "user", Content: "ping"}},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	perr := requireProviderError(t, err)
	if perr.Kind != llmtypes.KindAuth {
		t.Errorf("Kind = %q, want %q", perr.Kind, llmtypes.KindAuth)
	}
	if !strings.Contains(perr.Message, "invalid api key") {
		t.Errorf("Message = %q, want substring 'invalid api key'", perr.Message)
	}
	if perr.Provider != "opencode" {
		t.Errorf("Provider = %q, want opencode", perr.Provider)
	}
	if perr.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want 401", perr.StatusCode)
	}
}

func TestComplete_UpstreamErrorNonJSON(t *testing.T) {
	server := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream gateway down"))
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "k", HTTPClient: server.Client, Name: "opencode"})
	_, err := c.Complete(context.Background(), &llmtypes.Request{
		Model:    "deepseek-v4-flash",
		Messages: []llmtypes.Message{{Role: "user", Content: "ping"}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	perr := requireProviderError(t, err)
	if perr.Kind != llmtypes.KindUpstream {
		t.Errorf("Kind = %q, want %q", perr.Kind, llmtypes.KindUpstream)
	}
	if perr.Message != "upstream unavailable" {
		t.Errorf("Message = %q, want sanitized upstream message", perr.Message)
	}
	if !strings.Contains(string(perr.Raw), "upstream gateway down") {
		t.Errorf("Raw = %q, want original upstream body preserved for operators", string(perr.Raw))
	}
}

func TestComplete_InvalidSuccessBodySanitizesDecodeDetail(t *testing.T) {
	server := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html>internal stack from upstream</html>`))
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "k", HTTPClient: server.Client, Name: "opencode"})
	_, err := c.Complete(context.Background(), &llmtypes.Request{
		Model:    "deepseek-v4-flash",
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

func TestComplete_ValidationErrors(t *testing.T) {
	c := mustNew(t, Config{BaseURL: "http://example.invalid", APIKey: "k", Name: "opencode"})
	cases := []struct {
		name string
		req  *llmtypes.Request
	}{
		{"nil", nil},
		{"empty model", &llmtypes.Request{Messages: []llmtypes.Message{{Role: "user", Content: "x"}}}},
		{"empty messages", &llmtypes.Request{Model: "m"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Complete(context.Background(), tc.req)
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			perr := requireProviderError(t, err)
			if perr.Kind != llmtypes.KindBadRequest {
				t.Fatalf("Kind = %q, want %q", perr.Kind, llmtypes.KindBadRequest)
			}
		})
	}
}

// TestClassify_StatusAndEnvelope drives the classify helper directly so we
// can pin every HTTP-status / envelope-shape mapping in one place. New
// cases should land here before being touched in other tests.
