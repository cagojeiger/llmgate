package anthropic

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

func (c *Client) classify(status int, body []byte, retryAfterHeader string) *provider.Error {
	message, errorType := envelopeMessage(body)
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
	case status == http.StatusTooManyRequests:
		kind = provider.KindRateLimit
	case status == 529, status >= 500 && status <= 599:
		kind = provider.KindUpstream
	case status == http.StatusBadRequest, status == http.StatusUnprocessableEntity:
		kind = provider.KindBadRequest
		if looksLikeContextLength(message) {
			kind = provider.KindContextLength
		}
	}
	if kind == provider.KindUnknown && errorType != "" {
		kind = kindFromAnthropicErrorType(errorType)
	}

	return &provider.Error{
		Kind:       kind,
		Provider:   c.cfg.Name,
		Message:    message,
		StatusCode: status,
		RetryAfter: parseRetryAfter(retryAfterHeader),
		Raw:        firstBytes(body),
	}
}

func kindFromAnthropicErrorType(errorType string) provider.Kind {
	switch strings.ToLower(errorType) {
	case "authentication_error", "permission_error":
		return provider.KindAuth
	case "invalid_request_error", "not_found_error", "request_too_large":
		return provider.KindBadRequest
	case "rate_limit_error":
		return provider.KindRateLimit
	case "overloaded_error", "api_error":
		return provider.KindUpstream
	default:
		return provider.KindUpstream
	}
}

func errorFromStreamEvent(payload []byte, providerName string) *provider.Error {
	message, errorType := envelopeMessage(payload)
	if message == "" {
		message = "upstream stream error"
	}
	return &provider.Error{
		Kind:     kindFromAnthropicErrorType(errorType),
		Provider: providerName,
		Message:  message,
		Raw:      firstBytes(payload),
	}
}

func envelopeMessage(body []byte) (string, string) {
	var anthropicEnv struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &anthropicEnv); err == nil &&
		anthropicEnv.Type == "error" &&
		anthropicEnv.Error.Message != "" {
		return anthropicEnv.Error.Message, anthropicEnv.Error.Type
	}

	var openAIEnv struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &openAIEnv); err != nil {
		return "", ""
	}
	return openAIEnv.Error.Message, openAIEnv.Error.Type
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

func (c *Client) lowLevelError(message string, cause error) *provider.Error {
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
		Provider: c.cfg.Name,
		Message:  message + ": " + cause.Error(),
		Cause:    cause,
	}
}

func (c *Client) badRequest(message string, cause error, raw []byte) *provider.Error {
	return &provider.Error{
		Kind:     provider.KindBadRequest,
		Provider: c.cfg.Name,
		Message:  message + ": " + cause.Error(),
		Cause:    cause,
		Raw:      firstBytes(raw),
	}
}

func looksLikeContextLength(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "context_length") ||
		strings.Contains(lower, "context length") ||
		strings.Contains(lower, "token limit")
}

func firstBytes(b []byte) []byte {
	if len(b) > 256 {
		b = b[:256]
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
