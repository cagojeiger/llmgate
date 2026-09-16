package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/platform/upstream"
)

type EmbeddingConfig struct {
	BaseURL    string
	APIKey     string
	AuthScheme string
	Name       string
	HTTPClient *http.Client
}

type EmbeddingClient struct {
	cfg  EmbeddingConfig
	http *http.Client
}

func NewEmbedding(cfg EmbeddingConfig) (*EmbeddingClient, error) {
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.BaseURL == "" {
		return nil, errors.New("openai: BaseURL is required")
	}
	if cfg.Name == "" {
		cfg.Name = "openai"
	}
	if cfg.AuthScheme == "" {
		cfg.AuthScheme = "bearer"
	}
	if cfg.AuthScheme != "bearer" && cfg.AuthScheme != "x-api-key" {
		return nil, errors.New("unsupported embedding auth scheme")
	}
	client := cfg.HTTPClient
	if client == nil {
		client = upstream.DefaultClient()
	}
	return &EmbeddingClient{cfg: cfg, http: client}, nil
}

func (c *EmbeddingClient) Name() string { return c.cfg.Name }

func (c *EmbeddingClient) Embed(ctx context.Context, req *llmtypes.EmbeddingRequest) (*llmtypes.EmbeddingResponse, error) {
	if err := req.Validate(); err != nil {
		return nil, llmtypes.StampProvider(err, c.cfg.Name)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, upstream.BadRequest(c.cfg.Name, "encode embedding request", err, nil)
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, upstream.BadRequest(c.cfg.Name, "build embedding request", err, nil)
	}
	// A fresh HTTP/1 connection means a fresh RelayGate Pipe and pool selection.
	r.Close = true
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json")
	r.Header.Set("User-Agent", defaultUserAgent)
	if c.cfg.APIKey != "" {
		if c.cfg.AuthScheme == "x-api-key" {
			r.Header.Set("X-Api-Key", c.cfg.APIKey)
		} else {
			r.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
		}
	}
	resp, err := c.http.Do(r)
	if err != nil {
		return nil, upstream.LowLevelError(c.cfg.Name, "embedding request", err)
	}
	defer resp.Body.Close()
	const maxResponseBytes = 32 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, upstream.LowLevelError(c.cfg.Name, "read embeddings", err)
	}
	if len(raw) > maxResponseBytes {
		return nil, c.invalidResponse("embedding response exceeds 32 MiB")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, classifyError(c.cfg.Name, resp.StatusCode, raw, resp.Header.Get("Retry-After"))
	}
	var out llmtypes.EmbeddingResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, c.invalidResponse("invalid embedding response JSON")
	}
	inputs, _ := req.Inputs()
	if err := validateEmbeddings(&out, req, len(inputs)); err != nil {
		return nil, c.invalidResponse(err.Error())
	}
	return &out, nil
}

func (c *EmbeddingClient) invalidResponse(message string) error {
	return &llmtypes.Error{Kind: llmtypes.KindUpstream, Provider: c.cfg.Name, Message: message}
}
