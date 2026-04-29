package opencode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llmgate/internal/provider"
)

func TestComplete_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

		body, _ := io.ReadAll(r.Body)
		var got provider.Request
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}
		if got.Model != "deepseek-v4-flash" {
			t.Errorf("model = %q, want deepseek-v4-flash", got.Model)
		}
		if len(got.Messages) != 1 || got.Messages[0].Content != "ping" {
			t.Errorf("messages = %+v, want [{user,ping}]", got.Messages)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "chat-1",
			"model": "deepseek-v4-flash",
			"choices": [{
				"index": 0,
				"message": {"role": "assistant", "content": "pong"},
				"finish_reason": "stop"
			}],
			"usage": {"prompt_tokens": 5, "completion_tokens": 1, "total_tokens": 6}
		}`))
	}))
	defer server.Close()

	c := New("test-key", WithBaseURL(server.URL))
	resp, err := c.Complete(context.Background(), &provider.Request{
		Model:     "deepseek-v4-flash",
		Messages:  []provider.Message{{Role: "user", Content: "ping"}},
		MaxTokens: 32,
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
	if resp.Usage == nil || resp.Usage.TotalTokens != 6 {
		t.Errorf("usage = %+v, want TotalTokens=6", resp.Usage)
	}
}

func TestComplete_UpstreamErrorEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key","type":"authentication_error"}}`))
	}))
	defer server.Close()

	c := New("bad-key", WithBaseURL(server.URL))
	_, err := c.Complete(context.Background(), &provider.Request{
		Model:    "deepseek-v4-flash",
		Messages: []provider.Message{{Role: "user", Content: "ping"}},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	perr, ok := err.(*provider.Error)
	if !ok {
		t.Fatalf("err type = %T, want *provider.Error", err)
	}
	if !strings.Contains(perr.Message, "invalid api key") {
		t.Errorf("Message = %q, want substring 'invalid api key'", perr.Message)
	}
	if perr.Type != "authentication_error" {
		t.Errorf("Type = %q, want authentication_error", perr.Type)
	}
	if perr.Status != http.StatusUnauthorized {
		t.Errorf("Status = %d, want 401", perr.Status)
	}
}

func TestComplete_UpstreamErrorNonJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream gateway down"))
	}))
	defer server.Close()

	c := New("k", WithBaseURL(server.URL))
	_, err := c.Complete(context.Background(), &provider.Request{
		Model:    "deepseek-v4-flash",
		Messages: []provider.Message{{Role: "user", Content: "ping"}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	perr, ok := err.(*provider.Error)
	if !ok {
		t.Fatalf("err type = %T, want *provider.Error", err)
	}
	if perr.Type != "upstream_error" {
		t.Errorf("Type = %q, want upstream_error", perr.Type)
	}
	if !strings.Contains(perr.Message, "upstream gateway down") {
		t.Errorf("Message = %q, want substring 'upstream gateway down'", perr.Message)
	}
}

func TestComplete_ValidationErrors(t *testing.T) {
	c := New("k")
	cases := []struct {
		name string
		req  *provider.Request
	}{
		{"nil", nil},
		{"empty model", &provider.Request{Messages: []provider.Message{{Role: "user", Content: "x"}}}},
		{"empty messages", &provider.Request{Model: "m"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.Complete(context.Background(), tc.req); err == nil {
				t.Fatal("expected validation error, got nil")
			}
		})
	}
}
