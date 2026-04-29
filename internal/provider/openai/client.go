// Package openai adapts OpenAI-compatible upstreams to provider.Provider.
package openai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
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
	baseURL      string
	apiKey       string
	authScheme   string
	userAgent    string
	extraHeaders map[string]string
	name         string
	http         *http.Client
}

func New(cfg Config) (*Client, error) {
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		return nil, errors.New("openai: BaseURL is required")
	}
	if cfg.APIKey == "" {
		return nil, errors.New("openai: APIKey is required")
	}
	authScheme := strings.ToLower(cfg.AuthScheme)
	if authScheme == "" {
		authScheme = "bearer"
	}
	if authScheme != "bearer" && authScheme != "x-api-key" {
		return nil, fmt.Errorf("openai: unsupported AuthScheme %q", cfg.AuthScheme)
	}
	userAgent := cfg.UserAgent
	if userAgent == "" {
		userAgent = defaultUserAgent
	}
	name := cfg.Name
	if name == "" {
		name = "openai"
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = defaultHTTPClient()
	}

	c := &Client{
		baseURL:      baseURL,
		apiKey:       cfg.APIKey,
		authScheme:   authScheme,
		userAgent:    userAgent,
		extraHeaders: copyHeaders(cfg.ExtraHeaders),
		name:         name,
		http:         httpClient,
	}
	return c, nil
}

func defaultHTTPClient() *http.Client {
	return &http.Client{
		// No client-level timeout: LLM first byte can take minutes;
		// cancellation flows via the request context.
		Transport: &http.Transport{
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   50,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

func copyHeaders(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (c *Client) Name() string { return c.name }

func (c *Client) newRequest(ctx context.Context, accept string, body []byte) (*http.Request, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", accept)
	httpReq.Header.Set("User-Agent", c.userAgent)
	for k, v := range c.extraHeaders {
		httpReq.Header.Set(k, v)
	}
	switch c.authScheme {
	case "bearer":
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	case "x-api-key":
		httpReq.Header.Set("X-Api-Key", c.apiKey)
	}
	return httpReq, nil
}
