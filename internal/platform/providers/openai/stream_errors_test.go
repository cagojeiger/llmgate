package openai

import (
	"testing"

	"llmgate/internal/domain/llmtypes"
)

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
