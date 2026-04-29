package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

const (
	defaultBaseURL      = "https://opencode.ai/zen/go/v1"
	defaultDefaultModel = "deepseek-v4-flash"
)

// Provider is the minimum config any consumer of the provider library needs:
// upstream credentials and a default model. The HTTP server will embed this
// once it lands; for V1 the probe CLI is the only caller.
type Provider struct {
	OpenCodeBaseURL string
	OpenCodeAPIKey  string
	DefaultModel    string
}

func LoadProvider() (*Provider, error) {
	p := &Provider{
		OpenCodeBaseURL: orDefault("LLMGATE_OPENCODE_BASE_URL", defaultBaseURL),
		OpenCodeAPIKey:  os.Getenv("LLMGATE_OPENCODE_API_KEY"),
		DefaultModel:    orDefault("LLMGATE_DEFAULT_MODEL", defaultDefaultModel),
	}
	if p.OpenCodeAPIKey == "" {
		return nil, errors.New("LLMGATE_OPENCODE_API_KEY is required")
	}
	p.OpenCodeBaseURL = strings.TrimRight(p.OpenCodeBaseURL, "/")
	u, err := url.Parse(p.OpenCodeBaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("LLMGATE_OPENCODE_BASE_URL must be an absolute URL, got %q", p.OpenCodeBaseURL)
	}
	return p, nil
}

func orDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
