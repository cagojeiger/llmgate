// Package openai adapts OpenAI-compatible upstreams to llmtypes.Provider.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/platform/upstream"
)

const defaultUserAgent = "llmgate/0.1"

type Config struct {
	BaseURL    string
	APIKey     string
	AuthScheme string
	UserAgent  string
	HTTPClient *http.Client
	Name       string
	ExtraBody  map[string]any // default extra parameters to include in request body
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
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = upstream.DefaultClient()
	}
	return &Client{cfg: cfg, http: httpClient}, nil
}

func (c *Client) Name() string { return c.cfg.Name }

// marshalRequest marshals the request to JSON, merging in any default
// extra_body parameters configured for the model.
func (c *Client) marshalRequest(req *llmtypes.Request) ([]byte, error) {
	if len(c.cfg.ExtraBody) == 0 {
		return json.Marshal(req)
	}

	// Create a copy of the request to avoid mutating the original
	reqCopy := *req

	// Initialize Extra map if nil
	if reqCopy.Extra == nil {
		reqCopy.Extra = make(map[string]json.RawMessage)
	}

	// Merge model defaults (ExtraBody) with user-provided Extra
	// User values take precedence
	for k, v := range c.cfg.ExtraBody {
		if _, exists := reqCopy.Extra[k]; !exists {
			data, err := json.Marshal(v)
			if err != nil {
				return nil, fmt.Errorf("marshal extra_body field %q: %w", k, err)
			}
			reqCopy.Extra[k] = data
		}
	}

	return json.Marshal(&reqCopy)
}

func (c *Client) marshalRequestWithStream(req *llmtypes.Request) ([]byte, error) {
	reqCopy := *req
	t := true
	reqCopy.Stream = &t
	return c.marshalRequest(&reqCopy)
}

func (c *Client) newRequest(ctx context.Context, accept string, body []byte) (*http.Request, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", accept)
	httpReq.Header.Set("User-Agent", c.cfg.UserAgent)
	switch c.cfg.AuthScheme {
	case "bearer":
		httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	case "x-api-key":
		httpReq.Header.Set("X-Api-Key", c.cfg.APIKey)
	}
	return httpReq, nil
}
