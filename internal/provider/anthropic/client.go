// Package anthropic adapts Anthropic-compatible upstreams to provider.Provider.
package anthropic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"llmgate/internal/httpx"
)

const defaultUserAgent = "llmgate/0.1"

type Config struct {
	BaseURL          string
	APIKey           string
	AuthScheme       string
	UserAgent        string
	HTTPClient       *http.Client
	ExtraHeaders     map[string]string
	Name             string
	DefaultMaxTokens int
}

type Client struct {
	cfg  Config
	http *http.Client
}

func New(cfg Config) (*Client, error) {
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		return nil, errors.New("anthropic: BaseURL is required")
	}
	if cfg.APIKey == "" {
		return nil, errors.New("anthropic: APIKey is required")
	}
	authScheme := strings.ToLower(cfg.AuthScheme)
	if authScheme == "" {
		authScheme = "x-api-key"
	}
	if authScheme != "x-api-key" && authScheme != "bearer" {
		return nil, fmt.Errorf("anthropic: unsupported AuthScheme %q", cfg.AuthScheme)
	}
	userAgent := cfg.UserAgent
	if userAgent == "" {
		userAgent = defaultUserAgent
	}
	name := cfg.Name
	if name == "" {
		name = "anthropic"
	}
	defaultMaxTokens := cfg.DefaultMaxTokens
	if defaultMaxTokens == 0 {
		defaultMaxTokens = 4096
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = httpx.DefaultClient()
	}

	cfg.BaseURL = baseURL
	cfg.AuthScheme = authScheme
	cfg.UserAgent = userAgent
	cfg.ExtraHeaders = httpx.CopyHeaders(cfg.ExtraHeaders)
	cfg.Name = name
	cfg.DefaultMaxTokens = defaultMaxTokens

	return &Client{cfg: cfg, http: httpClient}, nil
}

func (c *Client) Name() string { return c.cfg.Name }

func (c *Client) newRequest(ctx context.Context, accept string, body []byte) (*http.Request, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", accept)
	httpReq.Header.Set("User-Agent", c.cfg.UserAgent)
	for k, v := range c.cfg.ExtraHeaders {
		httpReq.Header.Set(k, v)
	}
	switch c.cfg.AuthScheme {
	case "bearer":
		httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	case "x-api-key":
		httpReq.Header.Set("X-Api-Key", c.cfg.APIKey)
	}
	return httpReq, nil
}
