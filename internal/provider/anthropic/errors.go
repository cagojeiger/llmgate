package anthropic

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"llmgate/internal/provider"
	"llmgate/internal/provider/httpx"
)

func (c *Client) classify(status int, body []byte, retryAfterHeader string) *provider.Error {
	message, errorType := envelopeMessage(body)
	if message == "" {
		if len(body) > 0 {
			message = fmt.Sprintf("upstream returned status %d: %s", status, string(httpx.FirstBytes(body)))
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
	// content_filter overrides status-based classification — the envelope
	// is the authoritative signal, matching the OpenAI adapter's
	// isContentFilter behavior.
	if isAnthropicContentFilter(errorType) {
		kind = provider.KindContentFilter
	}

	return &provider.Error{
		Kind:       kind,
		Provider:   c.cfg.Name,
		Message:    message,
		StatusCode: status,
		RetryAfter: httpx.ParseRetryAfter(retryAfterHeader),
		Raw:        httpx.FirstBytes(body),
	}
}

func isAnthropicContentFilter(errorType string) bool {
	switch strings.ToLower(errorType) {
	case "content_filter", "content_filter_error":
		return true
	}
	return false
}

func kindFromAnthropicErrorType(errorType string) provider.Kind {
	switch strings.ToLower(errorType) {
	case "authentication_error", "permission_error":
		return provider.KindAuth
	case "invalid_request_error", "not_found_error", "request_too_large":
		return provider.KindBadRequest
	case "rate_limit_error":
		return provider.KindRateLimit
	case "content_filter", "content_filter_error":
		return provider.KindContentFilter
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
		Raw:      httpx.FirstBytes(payload),
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

func (c *Client) lowLevelError(message string, cause error) *provider.Error {
	return httpx.LowLevelError(c.cfg.Name, message, cause)
}

func (c *Client) badRequest(message string, cause error, raw []byte) *provider.Error {
	return httpx.BadRequest(c.cfg.Name, message, cause, raw)
}

func looksLikeContextLength(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "context_length") ||
		strings.Contains(lower, "context length") ||
		strings.Contains(lower, "token limit")
}
