// Package upstream holds the transport-level boilerplate every LLM
// provider adapter (openai, anthropic, future Gemini/Bedrock/...) needs
// when calling its upstream vendor: a hardened *http.Client default,
// uniform low-level / bad-request error wrapping, Retry-After parsing,
// and a body-trimming helper for audit Raw bytes. Vendor-specific
// classification (status → ErrorKind, envelope shape) stays in the
// adapter package — this layer only handles wire-protocol mechanics
// shared across vendors.
package upstream

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"llmgate/internal/domain/llmtypes"
)

// rawBodyLimit caps the raw-body bytes preserved on *llmtypes.Error.Raw
// so that audit logs never inherit megabyte-scale upstream payloads.
const rawBodyLimit = 256

// maxRetryAfter caps Retry-After delta-seconds before the int64
// nanosecond conversion overflows time.Duration (~292 years). A
// hostile or malformed header like `Retry-After: 99999999999` would
// otherwise produce a negative duration and defeat the negative
// clamp. A 24h ceiling is also more useful than "decades" for
// practical retry policies.
const maxRetryAfter = 24 * time.Hour

// HTTP transport pool sizing. Defaults match a single gateway
// fronting hundreds of concurrent upstream requests; the per-host
// pool is sized so a few vendor endpoints don't starve each other.
// http.Transport defaults (MaxIdleConnsPerHost=2) would force most
// requests to redial TLS on every call.
const (
	httpMaxIdleConns          = 100
	httpMaxIdleConnsPerHost   = 50
	httpIdleConnTimeout       = 90 * time.Second
	httpTLSHandshakeTimeout   = 10 * time.Second
	httpExpectContinueTimeout = 1 * time.Second
)

// DefaultClient builds an *http.Client tuned for LLM upstreams: no
// client-level timeout (first byte can take minutes — cancellation
// flows via the request context), and the pool constants above.
func DefaultClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:          httpMaxIdleConns,
			MaxIdleConnsPerHost:   httpMaxIdleConnsPerHost,
			IdleConnTimeout:       httpIdleConnTimeout,
			TLSHandshakeTimeout:   httpTLSHandshakeTimeout,
			ExpectContinueTimeout: httpExpectContinueTimeout,
		},
	}
}

// ParseRetryAfter decodes an HTTP Retry-After header into a duration,
// accepting both delta-seconds (RFC 9110: 1*DIGIT) and HTTP-date forms.
// Negative or malformed values clamp to 0 — RFC 9110 forbids negative
// delta-seconds, and surfacing a negative duration would mislead
// callers that pass it to time.Sleep / time.AfterFunc. Very large
// inputs are capped at maxRetryAfter to prevent int64 overflow.
func ParseRetryAfter(header string) time.Duration {
	if header == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(header, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		if seconds > int64(maxRetryAfter/time.Second) {
			return maxRetryAfter
		}
		return time.Duration(seconds) * time.Second
	} else if errors.Is(err, strconv.ErrRange) {
		// Valid integer literal but exceeds int64 range. Treat positives
		// as "very long backoff" (cap to maxRetryAfter) and negatives as
		// "no backoff" (clamp to 0) so a hostile huge value can't bypass
		// the cap by silently failing to parse.
		if strings.HasPrefix(header, "-") {
			return 0
		}
		return maxRetryAfter
	}
	if at, err := http.ParseTime(header); err == nil {
		d := time.Until(at)
		if d <= 0 {
			return 0
		}
		if d > maxRetryAfter {
			return maxRetryAfter
		}
		return d
	}
	return 0
}

// FirstBytes returns up to rawBodyLimit bytes of b in a freshly-allocated
// slice. The copy is intentional: callers store the slice on
// *llmtypes.Error.Raw (and audit logs), and we don't want them holding
// onto the upstream body buffer beyond the request lifetime.
func FirstBytes(b []byte) []byte {
	if len(b) > rawBodyLimit {
		b = b[:rawBodyLimit]
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// LowLevelError wraps a transport-level failure (DNS, TLS, connection
// refused, deadline exceeded) into a *llmtypes.Error with the right
// ErrorKind so callers don't have to sniff strings. ProviderName is stamped
// onto the error so audit logs and fallback policy can distinguish
// vendor sources.
func LowLevelError(providerName, message string, cause error) *llmtypes.Error {
	kind := llmtypes.KindNetwork
	if errors.Is(cause, context.DeadlineExceeded) {
		kind = llmtypes.KindTimeout
	} else {
		var netErr net.Error
		if errors.As(cause, &netErr) && netErr.Timeout() {
			kind = llmtypes.KindTimeout
		}
	}
	return &llmtypes.Error{
		Kind:     kind,
		Provider: providerName,
		Message:  message + ": " + cause.Error(),
		Cause:    cause,
	}
}

// BadRequest wraps a request-construction or marshal failure as a
// *llmtypes.Error with KindBadRequest. raw is trimmed via FirstBytes
// so audit logs stay bounded.
func BadRequest(providerName, message string, cause error, raw []byte) *llmtypes.Error {
	return &llmtypes.Error{
		Kind:     llmtypes.KindBadRequest,
		Provider: providerName,
		Message:  message + ": " + cause.Error(),
		Cause:    cause,
		Raw:      FirstBytes(raw),
	}
}

// PublicProviderMessage returns the message safe to expose on the OpenAI
// wire for provider-classified errors. Provider adapters should call this
// after kind classification: caller-actionable kinds keep their vendor
// envelope text, while opaque upstream failures collapse so raw vendor
// bodies, HTML error pages, hostnames, or stack details stay out of the
// response. The original body should still be preserved on llmtypes.Error.Raw
// for operator diagnostics.
func PublicProviderMessage(kind llmtypes.ErrorKind, message string) string {
	if kind == llmtypes.KindUpstream {
		return "upstream unavailable"
	}
	return message
}
