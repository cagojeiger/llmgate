package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"llmgate/internal/llmtypes"
)

// TestErrorPayload_TransportKindsCollapseToGeneric locks in the wire
// contract for failures that originate below the LLM contract: the
// JSON `message` field must NOT carry the original cause, because
// upstream/http.go's LowLevelError builds Message by concatenating
// cause.Error() — which can embed upstream IPs, in-cluster hostnames,
// or DNS errors.
//
// Operator detail is unchanged — rec.Kind on the audit row + the slog
// stream where the failure was observed still carry the full cause.
// Only the wire surface is sanitized.
//
// KindUpstream is deliberately omitted: that kind is set by provider
// adapters with deliberately-shaped messages, and sanitizing vendor
// body fragments belongs to the adapter layer, not this one. See the
// non-transport table below for the KindUpstream pass-through case.
func TestErrorPayload_TransportKindsCollapseToGeneric(t *testing.T) {
	cases := []struct {
		name        string
		err         *llmtypes.Error
		wantStatus  int
		wantMessage string
	}{
		{
			name: "network kind erases dial host:port",
			err: &llmtypes.Error{
				Kind:    llmtypes.KindNetwork,
				Message: "post chat: dial tcp 13.226.42.85:443: connect: connection refused",
			},
			wantStatus:  http.StatusBadGateway,
			wantMessage: "upstream unavailable",
		},
		{
			name: "timeout kind erases full upstream URL in cause chain",
			err: &llmtypes.Error{
				Kind:    llmtypes.KindTimeout,
				Message: "post chat: Post \"https://opencode.ai/zen/go/v1/chat/completions\": context deadline exceeded",
			},
			wantStatus:  http.StatusBadGateway,
			wantMessage: "upstream timeout",
		},
		{
			name: "empty kind erases vendor pod identifier",
			err: &llmtypes.Error{
				Kind:    llmtypes.KindEmpty,
				Message: "no completion from upstream pod 10.244.3.7",
			},
			wantStatus:  http.StatusBadGateway,
			wantMessage: "upstream unavailable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, _, payload := errorPayload(tc.err)
			if status != tc.wantStatus {
				t.Errorf("status = %d, want %d", status, tc.wantStatus)
			}

			var got map[string]any
			if err := json.Unmarshal(payload, &got); err != nil {
				t.Fatalf("unmarshal payload: %v (raw: %s)", err, payload)
			}
			errObj, ok := got["error"].(map[string]any)
			if !ok {
				t.Fatalf("payload missing error object: %s", payload)
			}
			gotMsg, _ := errObj["message"].(string)
			if gotMsg != tc.wantMessage {
				t.Errorf("wire message = %q, want %q", gotMsg, tc.wantMessage)
			}

			// Defense-in-depth: even if wantMessage matches, no
			// fragment of the original cause should slip through.
			// Catches a future regression that re-introduces
			// concatenation.
			leakNeedles := []string{
				"13.226.42.85",
				"opencode.ai",
				"10.244.3.7",
				"connection refused",
				"context deadline exceeded",
			}
			for _, needle := range leakNeedles {
				if strings.Contains(gotMsg, needle) {
					t.Errorf("wire message leaked %q: full message = %q", needle, gotMsg)
				}
			}

			if errObj["type"] != string(tc.err.Kind) {
				t.Errorf("error type = %v, want %q", errObj["type"], tc.err.Kind)
			}
		})
	}
}

// TestErrorPayload_NonTransportKindsPreserveMessage guards the inverse:
// kinds where the message IS the contract (caller-actionable info) must
// keep flowing to the wire so the caller can fix the request.
// KindUpstream is included here because the message there is
// adapter-shaped, not transport-cause concatenation.
func TestErrorPayload_NonTransportKindsPreserveMessage(t *testing.T) {
	cases := []struct {
		name        string
		err         *llmtypes.Error
		wantStatus  int
		wantMessage string
	}{
		{
			name:        "auth keeps the fixed wire string",
			err:         &llmtypes.Error{Kind: llmtypes.KindAuth, Message: "unauthorized"},
			wantStatus:  http.StatusUnauthorized,
			wantMessage: "unauthorized",
		},
		{
			name:        "bad_request preserves the parse offset",
			err:         &llmtypes.Error{Kind: llmtypes.KindBadRequest, Message: "decode request: invalid json at offset 12"},
			wantStatus:  http.StatusBadRequest,
			wantMessage: "decode request: invalid json at offset 12",
		},
		{
			name:        "context_length preserves the token count",
			err:         &llmtypes.Error{Kind: llmtypes.KindContextLength, Message: "prompt exceeds 200000 tokens"},
			wantStatus:  http.StatusBadRequest,
			wantMessage: "prompt exceeds 200000 tokens",
		},
		{
			name:        "content_filter preserves the block reason",
			err:         &llmtypes.Error{Kind: llmtypes.KindContentFilter, Message: "blocked: violence"},
			wantStatus:  http.StatusBadRequest,
			wantMessage: "blocked: violence",
		},
		{
			name:        "rate_limit preserves the limit caller hit",
			err:         &llmtypes.Error{Kind: llmtypes.KindRateLimit, Message: "exceeded 1000 RPM"},
			wantStatus:  http.StatusTooManyRequests,
			wantMessage: "exceeded 1000 RPM",
		},
		{
			name:        "upstream preserves adapter-shaped message",
			err:         &llmtypes.Error{Kind: llmtypes.KindUpstream, Message: "vendor responded 503"},
			wantStatus:  http.StatusBadGateway,
			wantMessage: "vendor responded 503",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, _, payload := errorPayload(tc.err)
			if status != tc.wantStatus {
				t.Errorf("status = %d, want %d", status, tc.wantStatus)
			}

			var got map[string]any
			if err := json.Unmarshal(payload, &got); err != nil {
				t.Fatalf("unmarshal payload: %v (raw: %s)", err, payload)
			}
			errObj, ok := got["error"].(map[string]any)
			if !ok {
				t.Fatalf("payload missing error object: %s", payload)
			}
			if errObj["message"] != tc.wantMessage {
				t.Errorf("wire message = %q, want %q", errObj["message"], tc.wantMessage)
			}
		})
	}
}
