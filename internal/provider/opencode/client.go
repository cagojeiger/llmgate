// Package opencode adapts the OpenCode Zen Go upstream to provider.Provider.
package opencode

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://opencode.ai/zen/go/v1"
	userAgent      = "llmgate/0.1"
)

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
			// No client-level timeout: LLM first byte can take minutes;
			// cancellation flows via the request context.
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

func (c *Client) newRequest(ctx context.Context, accept string, body []byte) (*http.Request, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", accept)
	httpReq.Header.Set("User-Agent", userAgent)
	return httpReq, nil
}
