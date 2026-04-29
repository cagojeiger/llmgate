package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	defaultBaseURL      = "https://opencode.ai/zen/go/v1"
	defaultDefaultModel = "deepseek-v4-flash"
)

// Provider is the minimum config any consumer of the provider library needs:
// upstream credentials and a default model.
type Provider struct {
	OpenCodeBaseURL string
	OpenCodeAPIKey  string
	DefaultModel    string
}

type Server struct {
	Provider
	Addr                  string
	ShutdownHeaderTimeout time.Duration
	ShutdownDrainTimeout  time.Duration
	LogLevel              slog.Level
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

func LoadServer() (*Server, error) {
	p, err := LoadProvider()
	if err != nil {
		return nil, err
	}

	headerTimeout, err := positiveDuration("LLMGATE_SHUTDOWN_HEADER_TIMEOUT", "3s")
	if err != nil {
		return nil, err
	}
	drainTimeout, err := positiveDuration("LLMGATE_SHUTDOWN_DRAIN_TIMEOUT", "7s")
	if err != nil {
		return nil, err
	}
	logLevel, err := parseLogLevel("LLMGATE_LOG_LEVEL", "info")
	if err != nil {
		return nil, err
	}

	return &Server{
		Provider:              *p,
		Addr:                  orDefault("LLMGATE_ADDR", ":8080"),
		ShutdownHeaderTimeout: headerTimeout,
		ShutdownDrainTimeout:  drainTimeout,
		LogLevel:              logLevel,
	}, nil
}

func positiveDuration(key, def string) (time.Duration, error) {
	raw := orDefault(key, def)
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration, got %q: %w", key, raw, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be > 0, got %q", key, raw)
	}
	return d, nil
}

func parseLogLevel(key, def string) (slog.Level, error) {
	raw := orDefault(key, def)
	var level slog.Level
	if err := level.UnmarshalText([]byte(raw)); err != nil {
		return 0, fmt.Errorf("%s must be a valid slog level, got %q: %w", key, raw, err)
	}
	return level, nil
}

func orDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
