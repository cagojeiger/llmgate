package openai

import (
	"net/http"
	"strings"
	"testing"

	"llmgate/internal/llmtypes"
)

func TestClassify_StatusAndEnvelope(t *testing.T) {
	c := mustNew(t, Config{BaseURL: "http://example.invalid", APIKey: "k", Name: "opencode"})
	cases := []struct {
		name   string
		status int
		body   string
		want   llmtypes.ErrorKind
	}{
		{"401 auth", 401, `{"error":{"message":"bad key","type":"authentication_error"}}`, llmtypes.KindAuth},
		{"403 auth", 403, `{"error":{"message":"forbidden"}}`, llmtypes.KindAuth},
		{"404 maps to bad_request", 404, `{"error":{"message":"no such model"}}`, llmtypes.KindBadRequest},
		{"408 request timeout", 408, `{"error":{"message":"server timeout"}}`, llmtypes.KindTimeout},
		{
			"413 request too large",
			413,
			`{"error":{"message":"payload too large","type":"request_too_large"}}`,
			llmtypes.KindBadRequest,
		},
		{
			"413 with token-limit hint becomes context_length",
			413,
			`{"error":{"message":"prompt exceeded token limit"}}`,
			llmtypes.KindContextLength,
		},
		{"422 unprocessable", 422, `{"error":{"message":"bad fields"}}`, llmtypes.KindBadRequest},
		{
			"400 with context_length hint",
			400,
			`{"error":{"message":"context length 8000 exceeded"}}`,
			llmtypes.KindContextLength,
		},
		{
			"400 content_filter via type",
			400,
			`{"error":{"message":"blocked","type":"content_filter"}}`,
			llmtypes.KindContentFilter,
		},
		{
			"400 content_filter via code",
			400,
			`{"error":{"message":"blocked","type":"invalid_request_error","code":"content_filter"}}`,
			llmtypes.KindContentFilter,
		},
		{"429 rate limit", 429, `{"error":{"message":"slow down"}}`, llmtypes.KindRateLimit},
		{"500 upstream", 500, `{"error":{"message":"internal"}}`, llmtypes.KindUpstream},
		{
			"529 overload (Anthropic-style status some gateways forward)",
			529,
			`{"error":{"message":"overloaded"}}`,
			llmtypes.KindUpstream,
		},
		{"non-string code does not break parsing", 400, `{"error":{"message":"bad","code":123}}`, llmtypes.KindBadRequest},
		{"unparseable body falls to status mapping", 502, `<html>oops</html>`, llmtypes.KindUpstream},
		{"unmapped 4xx remains unknown", 451, `{"error":{"message":"legal hold"}}`, llmtypes.KindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			perr := c.classify(tc.status, []byte(tc.body), "")
			if perr.Kind != tc.want {
				t.Errorf("Kind = %q, want %q (body=%s)", perr.Kind, tc.want, tc.body)
			}
			if perr.StatusCode != tc.status {
				t.Errorf("StatusCode = %d, want %d (preserved verbatim)", perr.StatusCode, tc.status)
			}
		})
	}
}

func TestClassify_UpstreamMessageSanitizedPreservesRaw(t *testing.T) {
	c := mustNew(t, Config{BaseURL: "http://example.invalid", APIKey: "k", Name: "opencode"})
	body := []byte(`{
		"error": {
			"message": "nginx upstream http://internal-opencode.svc.cluster.local:8080 failed",
			"type": "server_error"
		}
	}`)

	perr := c.classify(http.StatusBadGateway, body, "")
	if perr.Kind != llmtypes.KindUpstream {
		t.Fatalf("Kind = %q, want %q", perr.Kind, llmtypes.KindUpstream)
	}
	if perr.Message != "upstream unavailable" {
		t.Fatalf("Message = %q, want sanitized upstream message", perr.Message)
	}
	for _, needle := range []string{"internal-opencode", ".svc", ":8080", "nginx upstream"} {
		if strings.Contains(perr.Message, needle) {
			t.Fatalf("Message leaked %q: %q", needle, perr.Message)
		}
	}
	if string(perr.Raw) != string(body) {
		t.Fatalf("Raw = %q, want original body preserved", string(perr.Raw))
	}
}

func TestParseStreamError_UsesOpenAIKindClassifier(t *testing.T) {
	cases := []struct {
		name string
		body string
		want llmtypes.ErrorKind
	}{
		{"auth type", `{"error":{"message":"bad key","type":"authentication_error"}}`, llmtypes.KindAuth},
		{"rate type", `{"error":{"message":"slow","type":"rate_limit_error"}}`, llmtypes.KindRateLimit},
		{
			"context code",
			`{"error":{"message":"too long","type":"invalid_request_error","code":"context_length_exceeded"}}`,
			llmtypes.KindContextLength,
		},
		{
			"content filter code",
			`{"error":{"message":"blocked","type":"invalid_request_error","code":"content_filter"}}`,
			llmtypes.KindContentFilter,
		},
		{"invalid type", `{"error":{"message":"bad field","type":"invalid_request_error"}}`, llmtypes.KindBadRequest},
		{"unknown stream envelope", `{"error":{"message":"boom","type":"future_unknown"}}`, llmtypes.KindUpstream},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			perr := parseStreamError([]byte(tc.body), "opencode")
			if perr == nil {
				t.Fatal("parseStreamError returned nil")
			}
			if perr.Kind != tc.want {
				t.Fatalf("Kind = %q, want %q", perr.Kind, tc.want)
			}
		})
	}
}

// TestCompleteStream_NaturalEOFWithoutDone exercises the lenient
// terminator policy: an upstream that delivers events but ends the
// stream without the OpenAI `[DONE]` sentinel produces a clean io.EOF
// rather than a synthesized "missing [DONE]" error. This keeps the
// SSE reader interoperable with vendors (Anthropic) that don't emit
// `[DONE]` at all.
