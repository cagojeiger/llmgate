package anthropic

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"llmgate/internal/domain/llmtypes"
)

func TestClassify_ContentFilterOverridesStatus(t *testing.T) {
	c := mustNew(t, Config{BaseURL: "http://example.invalid", APIKey: "k", Name: "opencode"})
	cases := []struct {
		name   string
		status int
		body   string
		want   llmtypes.ErrorKind
	}{
		{
			name:   "400 + content_filter",
			status: 400,
			body:   `{"type":"error","error":{"type":"content_filter","message":"blocked by policy"}}`,
			want:   llmtypes.KindContentFilter,
		},
		{
			name:   "422 + content_filter_error",
			status: 422,
			body:   `{"type":"error","error":{"type":"content_filter_error","message":"blocked"}}`,
			want:   llmtypes.KindContentFilter,
		},
		{
			name:   "400 + invalid_request_error stays bad_request",
			status: 400,
			body:   `{"type":"error","error":{"type":"invalid_request_error","message":"bad field"}}`,
			want:   llmtypes.KindBadRequest,
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

// TestKindFromAnthropicErrorType pins the envelope error.type → ErrorKind
// mapping. Update this table when Anthropic ships new error types.
func TestKindFromAnthropicErrorType(t *testing.T) {
	cases := []struct {
		errorType string
		want      llmtypes.ErrorKind
	}{
		{"authentication_error", llmtypes.KindAuth},
		{"permission_error", llmtypes.KindAuth},
		{"invalid_request_error", llmtypes.KindBadRequest},
		{"not_found_error", llmtypes.KindBadRequest},
		{"request_too_large", llmtypes.KindBadRequest},
		{"rate_limit_error", llmtypes.KindRateLimit},
		{"content_filter", llmtypes.KindContentFilter},
		{"content_filter_error", llmtypes.KindContentFilter},
		{"overloaded_error", llmtypes.KindUpstream},
		{"api_error", llmtypes.KindUpstream},
		{"future_unknown_2030", llmtypes.KindUpstream},
	}
	for _, tc := range cases {
		t.Run(tc.errorType, func(t *testing.T) {
			if got := kindFromAnthropicErrorType(tc.errorType); got != tc.want {
				t.Errorf("Kind = %q, want %q", got, tc.want)
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
	_, err := c.Complete(context.Background(), &llmtypes.Request{
		Model:    "minimax-m2.5",
		Messages: []llmtypes.Message{{Role: "user", Content: "ping"}},
	})
	perr := requireProviderError(t, err)
	if perr.Kind != llmtypes.KindAuth {
		t.Errorf("Kind = %q, want %q", perr.Kind, llmtypes.KindAuth)
	}
	if !strings.Contains(perr.Message, "invalid api key") {
		t.Errorf("Message = %q, want invalid api key", perr.Message)
	}
	if perr.Provider != "opencode" {
		t.Errorf("Provider = %q, want opencode", perr.Provider)
	}
}

func TestClassify_UpstreamMessageSanitizedPreservesRaw(t *testing.T) {
	c := mustNew(t, Config{BaseURL: "http://example.invalid", APIKey: "k", Name: "opencode"})
	body := []byte(`{
		"type": "error",
		"error": {
			"type": "api_error",
			"message": "nginx upstream http://anthropic-proxy.default.svc:8080 failed"
		}
	}`)

	perr := c.classify(http.StatusBadGateway, body, "")
	if perr.Kind != llmtypes.KindUpstream {
		t.Fatalf("Kind = %q, want %q", perr.Kind, llmtypes.KindUpstream)
	}
	if perr.Message != "upstream unavailable" {
		t.Fatalf("Message = %q, want sanitized upstream message", perr.Message)
	}
	for _, needle := range []string{"anthropic-proxy", ".svc", ":8080", "nginx upstream"} {
		if strings.Contains(perr.Message, needle) {
			t.Fatalf("Message leaked %q: %q", needle, perr.Message)
		}
	}
	if string(perr.Raw) != string(body) {
		t.Fatalf("Raw = %q, want original body preserved", string(perr.Raw))
	}
}
