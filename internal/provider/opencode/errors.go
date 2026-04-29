package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"llmgate/internal/provider"
)

// classify maps HTTP status + upstream error envelope into a typed
// *provider.Error. Order: explicit envelope message > status-code mapping
// > generic fallback.
func classify(status int, body []byte, retryAfterHeader string) *provider.Error {
	message := envelopeMessage(body)
	if message == "" {
		if len(body) > 0 {
			message = fmt.Sprintf("upstream returned status %d: %s", status, string(firstBytes(body)))
		} else {
			message = fmt.Sprintf("upstream returned status %d", status)
		}
	}

	kind := provider.KindUnknown
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		kind = provider.KindAuth
	case status == http.StatusNotFound:
		kind = provider.KindBadRequest
	case status == http.StatusBadRequest, status == http.StatusUnprocessableEntity:
		kind = provider.KindBadRequest
		lower := strings.ToLower(message)
		if strings.Contains(lower, "token limit") || strings.Contains(lower, "context length") {
			kind = provider.KindContextLength
		}
	case status == http.StatusTooManyRequests:
		kind = provider.KindRateLimit
	case status == 529, status >= 500 && status <= 599:
		kind = provider.KindUpstream
	}

	return &provider.Error{
		Kind:       kind,
		Provider:   "opencode",
		Message:    message,
		StatusCode: status,
		RetryAfter: parseRetryAfter(retryAfterHeader),
		Raw:        firstBytes(body),
	}
}

func envelopeMessage(body []byte) string {
	var env struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	return env.Error.Message
}

func kindFromErrorType(errorType, message string) provider.Kind {
	lowerType := strings.ToLower(errorType)
	lowerMessage := strings.ToLower(message)
	switch {
	case strings.Contains(lowerType, "auth"):
		return provider.KindAuth
	case strings.Contains(lowerType, "rate"):
		return provider.KindRateLimit
	case strings.Contains(lowerType, "context") ||
		strings.Contains(lowerMessage, "token limit") ||
		strings.Contains(lowerMessage, "context length"):
		return provider.KindContextLength
	case strings.Contains(lowerType, "content_filter"):
		return provider.KindContentFilter
	case strings.Contains(lowerType, "invalid"):
		return provider.KindBadRequest
	case strings.Contains(lowerType, "upstream"):
		return provider.KindUpstream
	}
	return provider.KindUpstream
}

func parseRetryAfter(header string) time.Duration {
	if header == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(header); err == nil {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(header); err == nil {
		d := time.Until(at)
		if d > 0 {
			return d
		}
	}
	return 0
}

// lowLevelError wraps a transport-level error (DNS, TLS, conn refused,
// timeout) into a *provider.Error with the right Kind so callers can
// switch on it without sniffing strings.
func lowLevelError(message string, cause error) *provider.Error {
	kind := provider.KindNetwork
	if errors.Is(cause, context.DeadlineExceeded) {
		kind = provider.KindTimeout
	} else {
		var netErr net.Error
		if errors.As(cause, &netErr) && netErr.Timeout() {
			kind = provider.KindTimeout
		}
	}
	return &provider.Error{
		Kind:     kind,
		Provider: "opencode",
		Message:  message + ": " + cause.Error(),
		Cause:    cause,
	}
}

func badRequest(message string, cause error, raw []byte) *provider.Error {
	return &provider.Error{
		Kind:     provider.KindBadRequest,
		Provider: "opencode",
		Message:  message + ": " + cause.Error(),
		Cause:    cause,
		Raw:      firstBytes(raw),
	}
}

// withProvider stamps "opencode" onto a *provider.Error coming from a
// shared helper (e.g. provider.NewSSEReader) so callers always see the
// originating adapter.
func withProvider(err error) error {
	var perr *provider.Error
	if !errors.As(err, &perr) {
		return err
	}
	if perr.Provider == "opencode" {
		return perr
	}
	stamped := *perr
	stamped.Provider = "opencode"
	return &stamped
}

func firstBytes(b []byte) []byte {
	if len(b) > 256 {
		b = b[:256]
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
