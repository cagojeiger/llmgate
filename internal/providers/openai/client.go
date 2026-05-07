// Package openai adapts OpenAI-compatible upstreams to llmtypes.Provider.
package openai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"llmgate/internal/upstream"
)

const defaultUserAgent = "llmgate/0.1"

type Config struct {
	BaseURL      string
	APIKey       string
	AuthScheme   string
	UserAgent    string
	HTTPClient   *http.Client
	ExtraHeaders map[string]string
	Name         string
}

type Client struct {
	cfg  Config
	http *http.Client
}

func New(cfg Config) (*Client, error) {
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.BaseURL == "" {
		return nil, errors.New("openai: BaseURL is required")
	}
	if cfg.APIKey == "" {
		return nil, errors.New("openai: APIKey is required")
	}
	cfg.AuthScheme = strings.ToLower(cfg.AuthScheme)
	if cfg.AuthScheme == "" {
		cfg.AuthScheme = "bearer"
	}
	if cfg.AuthScheme != "bearer" && cfg.AuthScheme != "x-api-key" {
		return nil, fmt.Errorf("openai: unsupported AuthScheme %q", cfg.AuthScheme)
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = defaultUserAgent
	}
	if cfg.Name == "" {
		cfg.Name = "openai"
	}
	cfg.ExtraHeaders = upstream.CopyHeaders(cfg.ExtraHeaders)
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = upstream.DefaultClient()
	}
	return &Client{cfg: cfg, http: httpClient}, nil
}

func (c *Client) Name() string { return c.cfg.Name }

func (c *Client) newRequest(ctx context.Context, accept string, body []byte) (*http.Request, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
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
