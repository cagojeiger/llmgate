package openai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"llmgate/internal/provider"
	"llmgate/internal/upstream"
)

// classify maps HTTP status + upstream error envelope into a typed
// *provider.Error. Order: explicit envelope message > status-code mapping
// > generic fallback. The envelope's `type` and `code` fields can refine
// the kind when the status alone is ambiguous (most importantly,
// `content_filter` — OpenAI gateways encode policy blocks via the
// envelope, not via a dedicated status code).
func (c *Client) classify(status int, body []byte, retryAfterHeader string) *provider.Error {
	message, errorType, errorCode := envelopeMessage(body)
	if message == "" {
		if len(body) > 0 {
			message = fmt.Sprintf("upstream returned status %d: %s", status, string(upstream.FirstBytes(body)))
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
	case status == http.StatusRequestTimeout:
		kind = provider.KindTimeout
	case status == http.StatusBadRequest,
		status == http.StatusUnprocessableEntity,
		status == http.StatusRequestEntityTooLarge:
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

	if isContentFilter(errorType, errorCode) {
		kind = provider.KindContentFilter
	}

	return &provider.Error{
		Kind:       kind,
		Provider:   c.cfg.Name,
		Message:    message,
		StatusCode: status,
		RetryAfter: upstream.ParseRetryAfter(retryAfterHeader),
		Raw:        upstream.FirstBytes(body),
	}
}

// envelopeMessage returns the OpenAI-style error envelope's message,
// type, and code (best-effort). `code` is decoded from RawMessage so a
// non-string value (some gateways send int / null) doesn't fail the
// whole unmarshal.
func envelopeMessage(body []byte) (message, errorType, errorCode string) {
	var env struct {
		Error struct {
			Message string          `json:"message"`
			Type    string          `json:"type"`
			Code    json.RawMessage `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return "", "", ""
	}
	if len(env.Error.Code) > 0 {
		_ = json.Unmarshal(env.Error.Code, &errorCode)
	}
	return env.Error.Message, env.Error.Type, errorCode
}

func isContentFilter(errorType, errorCode string) bool {
	return strings.EqualFold(errorType, "content_filter") ||
		strings.EqualFold(errorCode, "content_filter")
}

func (c *Client) lowLevelError(message string, cause error) *provider.Error {
	return upstream.LowLevelError(c.cfg.Name, message, cause)
}

func (c *Client) badRequest(message string, cause error, raw []byte) *provider.Error {
	return upstream.BadRequest(c.cfg.Name, message, cause, raw)
}
