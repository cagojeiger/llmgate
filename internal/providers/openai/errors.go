package openai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"llmgate/internal/llmtypes"
	"llmgate/internal/upstream"
)

// classify maps HTTP status + upstream error envelope into a typed
// *llmtypes.Error. Order: explicit envelope message > status-code mapping
// > generic fallback. The envelope's `type` and `code` fields can refine
// the kind when the status alone is ambiguous (most importantly,
// `content_filter` — OpenAI gateways encode policy blocks via the
// envelope, not via a dedicated status code).
func (c *Client) classify(status int, body []byte, retryAfterHeader string) *llmtypes.Error {
	env := parseErrorEnvelope(body)
	message := env.Message
	if message == "" {
		if len(body) > 0 {
			message = fmt.Sprintf("upstream returned status %d: %s", status, string(upstream.FirstBytes(body)))
		} else {
			message = fmt.Sprintf("upstream returned status %d", status)
		}
	}

	env.Message = message

	return &llmtypes.Error{
		Kind:       kindFromOpenAIError(status, env),
		Provider:   c.cfg.Name,
		Message:    message,
		StatusCode: status,
		RetryAfter: upstream.ParseRetryAfter(retryAfterHeader),
		Raw:        upstream.FirstBytes(body),
	}
}

type errorEnvelope struct {
	Message string
	Type    string
	Code    string
}

// parseErrorEnvelope returns the OpenAI-style error envelope's message,
// type, and code (best-effort). Code is decoded from RawMessage so a
// non-string value (some gateways send int/null) does not fail parsing.
func parseErrorEnvelope(body []byte) errorEnvelope {
	var env struct {
		Error struct {
			Message string          `json:"message"`
			Type    string          `json:"type"`
			Code    json.RawMessage `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return errorEnvelope{}
	}
	out := errorEnvelope{
		Message: env.Error.Message,
		Type:    env.Error.Type,
	}
	if len(env.Error.Code) > 0 {
		_ = json.Unmarshal(env.Error.Code, &out.Code)
	}
	return out
}

func kindFromOpenAIError(status int, env errorEnvelope) llmtypes.ErrorKind {
	t := strings.ToLower(env.Type)
	c := strings.ToLower(env.Code)
	m := strings.ToLower(env.Message)

	switch {
	case strings.EqualFold(env.Type, "content_filter") || strings.EqualFold(env.Code, "content_filter"):
		return llmtypes.KindContentFilter
	case strings.Contains(t, "auth"):
		return llmtypes.KindAuth
	case strings.Contains(t, "rate"):
		return llmtypes.KindRateLimit
	case strings.Contains(t, "context") || strings.Contains(c, "context") || strings.Contains(m, "token limit") || strings.Contains(m, "context length"):
		return llmtypes.KindContextLength
	case strings.Contains(t, "invalid"):
		return llmtypes.KindBadRequest
	}

	switch {
	case status == 0:
		return llmtypes.KindUpstream
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return llmtypes.KindAuth
	case status == http.StatusNotFound:
		return llmtypes.KindBadRequest
	case status == http.StatusRequestTimeout:
		return llmtypes.KindTimeout
	case status == http.StatusBadRequest,
		status == http.StatusUnprocessableEntity,
		status == http.StatusRequestEntityTooLarge:
		return llmtypes.KindBadRequest
	case status == http.StatusTooManyRequests:
		return llmtypes.KindRateLimit
	case status == 529, status >= 500 && status <= 599:
		return llmtypes.KindUpstream
	default:
		return llmtypes.KindUnknown
	}
}

func (c *Client) lowLevelError(message string, cause error) *llmtypes.Error {
	return upstream.LowLevelError(c.cfg.Name, message, cause)
}

func (c *Client) badRequest(message string, cause error, raw []byte) *llmtypes.Error {
	return upstream.BadRequest(c.cfg.Name, message, cause, raw)
}
