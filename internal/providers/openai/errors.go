package openai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"llmgate/internal/core"
	"llmgate/internal/upstream"
)

// classify maps HTTP status + upstream error envelope into a typed
// *core.Error. Order: explicit envelope message > status-code mapping
// > generic fallback. The envelope's `type` and `code` fields can refine
// the kind when the status alone is ambiguous (most importantly,
// `content_filter` — OpenAI gateways encode policy blocks via the
// envelope, not via a dedicated status code).
func (c *Client) classify(status int, body []byte, retryAfterHeader string) *core.Error {
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

	return &core.Error{
		ErrorKind:  kindFromOpenAIError(status, env),
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

func kindFromOpenAIError(status int, env errorEnvelope) core.ErrorKind {
	t := strings.ToLower(env.Type)
	c := strings.ToLower(env.Code)
	m := strings.ToLower(env.Message)

	switch {
	case strings.EqualFold(env.Type, "content_filter") || strings.EqualFold(env.Code, "content_filter"):
		return core.KindContentFilter
	case strings.Contains(t, "auth"):
		return core.KindAuth
	case strings.Contains(t, "rate"):
		return core.KindRateLimit
	case strings.Contains(t, "context") || strings.Contains(c, "context") || strings.Contains(m, "token limit") || strings.Contains(m, "context length"):
		return core.KindContextLength
	case strings.Contains(t, "invalid"):
		return core.KindBadRequest
	}

	switch {
	case status == 0:
		return core.KindUpstream
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return core.KindAuth
	case status == http.StatusNotFound:
		return core.KindBadRequest
	case status == http.StatusRequestTimeout:
		return core.KindTimeout
	case status == http.StatusBadRequest,
		status == http.StatusUnprocessableEntity,
		status == http.StatusRequestEntityTooLarge:
		return core.KindBadRequest
	case status == http.StatusTooManyRequests:
		return core.KindRateLimit
	case status == 529, status >= 500 && status <= 599:
		return core.KindUpstream
	default:
		return core.KindUnknown
	}
}

func (c *Client) lowLevelError(message string, cause error) *core.Error {
	return upstream.LowLevelError(c.cfg.Name, message, cause)
}

func (c *Client) badRequest(message string, cause error, raw []byte) *core.Error {
	return upstream.BadRequest(c.cfg.Name, message, cause, raw)
}
