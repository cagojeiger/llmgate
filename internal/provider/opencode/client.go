package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"llmgate/internal/provider"
)

const (
	defaultBaseURL = "https://opencode.ai/zen/go/v1"
	userAgent      = "llmgate/0.1"
)

// Client is the OpenCode Zen Go adapter. It speaks the OpenAI-compatible
// /chat/completions endpoint and surfaces errors as *provider.Error so
// callers don't need to sniff HTTP status separately.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

type Option func(*Client)

func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") }
}

func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

func New(apiKey string, opts ...Option) *Client {
	c := &Client{
		baseURL: defaultBaseURL,
		apiKey:  apiKey,
		http: &http.Client{
			// No client-level timeout: LLM first byte can be minutes.
			// Cancellation flows via the request context.
			Transport: &http.Transport{
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   50,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *Client) Name() string { return "opencode" }

func (c *Client) Complete(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("opencode: nil request")
	}
	if req.Model == "" {
		return nil, fmt.Errorf("opencode: request.Model is required")
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("opencode: request.Messages is empty")
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("opencode: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("opencode: build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", userAgent)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("opencode: send request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("opencode: read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, parseError(resp.StatusCode, raw)
	}

	var out provider.Response
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("opencode: decode response: %w (body: %s)", err, truncate(raw))
	}
	return &out, nil
}

// parseError tries to honor OpenAI's nested {"error":{...}} envelope first,
// then falls back to a synthetic *provider.Error when the body isn't JSON.
func parseError(status int, body []byte) error {
	var env struct {
		Error provider.Error `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Error.Message != "" {
		if env.Error.Type == "" {
			env.Error.Type = httpStatusType(status)
		}
		env.Error.Status = status
		return &env.Error
	}
	return &provider.Error{
		Message: fmt.Sprintf("upstream returned status %d: %s", status, truncate(body)),
		Type:    httpStatusType(status),
		Status:  status,
	}
}

func httpStatusType(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 400 && status < 500:
		return "invalid_request_error"
	case status >= 500:
		return "upstream_error"
	}
	return "unknown_error"
}

func truncate(b []byte) string {
	const max = 256
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "...(truncated)"
}
