package anthropic

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"llmgate/internal/llmtypes"
	"llmgate/internal/upstream"
)

func (c *Client) classify(status int, body []byte, retryAfterHeader string) *llmtypes.Error {
	message, errorType := envelopeMessage(body)
	if message == "" {
		if len(body) > 0 {
			message = fmt.Sprintf("upstream returned status %d: %s", status, string(upstream.FirstBytes(body)))
		} else {
			message = fmt.Sprintf("upstream returned status %d", status)
		}
	}

	kind := llmtypes.KindUnknown
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		kind = llmtypes.KindAuth
	case status == http.StatusTooManyRequests:
		kind = llmtypes.KindRateLimit
	case status == 529, status >= 500 && status <= 599:
		kind = llmtypes.KindUpstream
	case status == http.StatusBadRequest, status == http.StatusUnprocessableEntity:
		kind = llmtypes.KindBadRequest
		if looksLikeContextLength(message) {
			kind = llmtypes.KindContextLength
		}
	}
	if kind == llmtypes.KindUnknown && errorType != "" {
		kind = kindFromAnthropicErrorType(errorType)
	}
	// content_filter overrides status-based classification — the envelope
	// is the authoritative signal, matching the OpenAI adapter's
	// isContentFilter behavior.
	if isAnthropicContentFilter(errorType) {
		kind = llmtypes.KindContentFilter
	}

	return &llmtypes.Error{
		ErrorKind:  kind,
		Provider:   c.cfg.Name,
		Message:    message,
		StatusCode: status,
		RetryAfter: upstream.ParseRetryAfter(retryAfterHeader),
		Raw:        upstream.FirstBytes(body),
	}
}

func isAnthropicContentFilter(errorType string) bool {
	switch strings.ToLower(errorType) {
	case "content_filter", "content_filter_error":
		return true
	}
	return false
}

func kindFromAnthropicErrorType(errorType string) llmtypes.ErrorKind {
	switch strings.ToLower(errorType) {
	case "authentication_error", "permission_error":
		return llmtypes.KindAuth
	case "invalid_request_error", "not_found_error", "request_too_large":
		return llmtypes.KindBadRequest
	case "rate_limit_error":
		return llmtypes.KindRateLimit
	case "content_filter", "content_filter_error":
		return llmtypes.KindContentFilter
	case "overloaded_error", "api_error":
		return llmtypes.KindUpstream
	default:
		return llmtypes.KindUpstream
	}
}

func errorFromStreamEvent(payload []byte, providerName string) *llmtypes.Error {
	message, errorType := envelopeMessage(payload)
	if message == "" {
		message = "upstream stream error"
	}
	return &llmtypes.Error{
		ErrorKind: kindFromAnthropicErrorType(errorType),
		Provider:  providerName,
		Message:   message,
		Raw:       upstream.FirstBytes(payload),
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

func (c *Client) lowLevelError(message string, cause error) *llmtypes.Error {
	return upstream.LowLevelError(c.cfg.Name, message, cause)
}

func (c *Client) badRequest(message string, cause error, raw []byte) *llmtypes.Error {
	return upstream.BadRequest(c.cfg.Name, message, cause, raw)
}

func looksLikeContextLength(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "context_length") ||
		strings.Contains(lower, "context length") ||
		strings.Contains(lower, "token limit")
}
