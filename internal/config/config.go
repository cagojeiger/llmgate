package config

import (
	"fmt"
	"log/slog"
	"os"
	"time"
)

type Server struct {
	Addr                  string
	ShutdownHeaderTimeout time.Duration
	ShutdownDrainTimeout  time.Duration
	LogLevel              slog.Level
}

func LoadServer() (*Server, error) {
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
