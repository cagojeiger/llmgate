package server

import (
	"encoding/json"
	"errors"
	"fmt"
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
				Cause:   errors.New("dial tcp 13.226.42.85:443: connect: connection refused"),
			},
			wantStatus:  http.StatusBadGateway,
			wantMessage: "upstream unavailable",
		},
		{
			name: "timeout kind erases full upstream URL in cause chain",
			err: &llmtypes.Error{
				Kind:    llmtypes.KindTimeout,
				Message: "post chat: Post \"https://opencode.ai/zen/go/v1/chat/completions\": context deadline exceeded",
				Cause:   errors.New("context deadline exceeded"),
			},
			wantStatus:  http.StatusBadGateway,
			wantMessage: "upstream timeout",
		},
		{
			name: "empty kind from sse_reader scanner erases vendor pod identifier",
			err: &llmtypes.Error{
				Kind:    llmtypes.KindEmpty,
				Message: "no completion from upstream pod 10.244.3.7",
				Cause:   errors.New("read tcp 10.244.3.7:443: connection reset"),
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

// TestErrorPayload_AdapterClassifiedTransportKindsPreserveMessage guards
// against accidentally collapsing adapter-shaped messages on transport
// kinds. Adapter origins fall into two shapes:
//   - Vendor HTTP responses parsed from envelopes (408 → KindTimeout,
//     502 / 504 / etc.). Message is vendor-shaped; StatusCode is set
//     to the upstream HTTP status; Cause is nil.
//   - Adapter-built diagnostics like "empty response" for HTTP 200
//     with empty body. Message is a fixed string; StatusCode is 0;
//     Cause is nil.
//
// Distinguishing signal: low-level transport faults
// (upstream/http.go's LowLevelError, sse_reader's scanner.Err) ALWAYS
// wrap an underlying error in Cause; adapters never do. The wire
// collapse keys on Cause presence, so both adapter shapes flow through
// unchanged.
func TestErrorPayload_AdapterClassifiedTransportKindsPreserveMessage(t *testing.T) {
	cases := []struct {
		name        string
		err         *llmtypes.Error
		wantStatus  int
		wantMessage string
	}{
		{
			name: "timeout from adapter HTTP 408 preserves vendor envelope",
			err: &llmtypes.Error{
				Kind:       llmtypes.KindTimeout,
				StatusCode: http.StatusRequestTimeout,
				Message:    "server timeout",
			},
			wantStatus:  http.StatusBadGateway,
			wantMessage: "server timeout",
		},
		{
			name: "network from adapter HTTP 502 preserves vendor envelope",
			err: &llmtypes.Error{
				Kind:       llmtypes.KindNetwork,
				StatusCode: http.StatusBadGateway,
				Message:    "vendor reported bad gateway",
			},
			wantStatus:  http.StatusBadGateway,
			wantMessage: "vendor reported bad gateway",
		},
		{
			name: "empty from adapter HTTP 504 preserves vendor envelope",
			err: &llmtypes.Error{
				Kind:       llmtypes.KindEmpty,
				StatusCode: http.StatusGatewayTimeout,
				Message:    "vendor returned no completions",
			},
			wantStatus:  http.StatusBadGateway,
			wantMessage: "vendor returned no completions",
		},
		{
			// providers/openai/complete.go:42 and providers/anthropic/
			// complete.go:42 build this on HTTP 200 with an empty body.
			// StatusCode is 0 and Cause is nil — the discriminator is
			// Cause, not StatusCode.
			name: "empty diagnostic on HTTP 200 empty body preserves adapter string",
			err: &llmtypes.Error{
				Kind:     llmtypes.KindEmpty,
				Provider: "opencode",
				Message:  "empty response",
			},
			wantStatus:  http.StatusBadGateway,
			wantMessage: "empty response",
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
				t.Errorf("wire message = %q, want %q (adapter-shaped messages must not be collapsed)", errObj["message"], tc.wantMessage)
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

// TestErrorPayload_WrappedErrorsRoutedByChainCause guards the
// chain-walk contract for the Cause gate. errors that have been
// re-wrapped (fmt.Errorf("…: %w", typedErr)) at any layer must still
// route by the chain-found *llmtypes.Error's Cause, not by the
// outermost wrapper's Unwrap. This was a real regression in an
// earlier iteration that used errors.Unwrap on the top-level err and
// therefore mis-classified once-wrapped adapter errors as transport.
func TestErrorPayload_WrappedErrorsRoutedByChainCause(t *testing.T) {
	t.Run("wrapped adapter diagnostic stays adapter-shaped", func(t *testing.T) {
		// Adapter classified an HTTP 200 empty body, then a router
		// layer wrapped the typed error for context. Cause on the
		// inner *llmtypes.Error is still nil — must surface the
		// adapter message, not "upstream unavailable".
		inner := &llmtypes.Error{
			Kind:     llmtypes.KindEmpty,
			Provider: "opencode",
			Message:  "empty response",
		}
		wrapped := fmt.Errorf("route deepseek-v4-flash: %w", inner)

		_, _, payload := errorPayload(wrapped)
		var got map[string]any
		if err := json.Unmarshal(payload, &got); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		errObj := got["error"].(map[string]any)
		if errObj["message"] != "empty response" {
			t.Errorf("wire message = %q, want %q (adapter Cause=nil must survive a wrap)", errObj["message"], "empty response")
		}
	})

	t.Run("wrapped transport fault still collapses", func(t *testing.T) {
		// Transport fault (LowLevelError-shape) wrapped once more by
		// a routing layer. Inner *llmtypes.Error.Cause is set —
		// must still collapse to the generic wire message rather
		// than leaking the wrapper or cause detail.
		inner := &llmtypes.Error{
			Kind:    llmtypes.KindNetwork,
			Message: "post chat: dial tcp 13.226.42.85:443: connect: connection refused",
			Cause:   errors.New("dial tcp 13.226.42.85:443: connect: connection refused"),
		}
		wrapped := fmt.Errorf("route deepseek-v4-flash: %w", inner)

		_, _, payload := errorPayload(wrapped)
		var got map[string]any
		if err := json.Unmarshal(payload, &got); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		errObj := got["error"].(map[string]any)
		gotMsg, _ := errObj["message"].(string)
		if gotMsg != "upstream unavailable" {
			t.Errorf("wire message = %q, want %q (wrapped transport must still collapse)", gotMsg, "upstream unavailable")
		}
		if strings.Contains(gotMsg, "13.226.42.85") {
			t.Errorf("wire message leaked wrapped transport detail: %q", gotMsg)
		}
	})
}
