package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

type Server struct {
	Addr                  string
	ShutdownHeaderTimeout time.Duration
	ShutdownDrainTimeout  time.Duration
	LogLevel              slog.Level

	// FallbackOn / CircuitFailures / CircuitOpen tune the router's
	// retry behavior. They live here, not in catalog yaml, because
	// they describe gateway-internal algorithm settings — not vendor
	// or model data. main.go assembles them into a router.FallbackPolicy.
	// Defaults are sized for typical LLM upstreams (transient 429/5xx,
	// 3 strikes, 30s base cooldown with capped backoff); operators only
	// set the env vars when the defaults don't fit.
	FallbackOn             []string
	CircuitFailures        int
	CircuitOpen            time.Duration
	CircuitMaxOpen         time.Duration
	CircuitJitter          float64
	CompleteRequestTimeout time.Duration
	CompleteAttemptTimeout time.Duration
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
	circuitFailures, err := nonNegativeInt("LLMGATE_CIRCUIT_FAILURES", "3")
	if err != nil {
		return nil, err
	}
	circuitOpen, err := nonNegativeDuration("LLMGATE_CIRCUIT_OPEN_DURATION", "30s")
	if err != nil {
		return nil, err
	}
	circuitMaxOpen, err := nonNegativeDuration("LLMGATE_CIRCUIT_MAX_OPEN_DURATION", "5m")
	if err != nil {
		return nil, err
	}
	circuitJitter, err := ratio("LLMGATE_CIRCUIT_JITTER", "0.2")
	if err != nil {
		return nil, err
	}
	completeRequestTimeout, err := nonNegativeDuration("LLMGATE_COMPLETE_REQUEST_TIMEOUT", "3m")
	if err != nil {
		return nil, err
	}
	completeAttemptTimeout, err := nonNegativeDuration("LLMGATE_COMPLETE_ATTEMPT_TIMEOUT", "1m")
	if err != nil {
		return nil, err
	}

	return &Server{
		Addr:                   orDefault("LLMGATE_ADDR", ":8080"),
		ShutdownHeaderTimeout:  headerTimeout,
		ShutdownDrainTimeout:   drainTimeout,
		LogLevel:               logLevel,
		FallbackOn:             parseCSV("LLMGATE_FALLBACK_ON", "rate_limit,upstream,timeout,network"),
		CircuitFailures:        circuitFailures,
		CircuitOpen:            circuitOpen,
		CircuitMaxOpen:         circuitMaxOpen,
		CircuitJitter:          circuitJitter,
		CompleteRequestTimeout: completeRequestTimeout,
		CompleteAttemptTimeout: completeAttemptTimeout,
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

// nonNegativeDuration accepts 0 (= disabled) so operators can turn the
// circuit breaker off explicitly with LLMGATE_CIRCUIT_OPEN_DURATION=0s.
func nonNegativeDuration(key, def string) (time.Duration, error) {
	raw := orDefault(key, def)
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration, got %q: %w", key, raw, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s must be >= 0, got %q", key, raw)
	}
	return d, nil
}

// nonNegativeInt accepts 0 (= disabled) for the same reason as
// nonNegativeDuration: setting LLMGATE_CIRCUIT_FAILURES=0 disables the
// circuit breaker.
func nonNegativeInt(key, def string) (int, error) {
	raw := orDefault(key, def)
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer, got %q: %w", key, raw, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s must be >= 0, got %q", key, raw)
	}
	return n, nil
}

func ratio(key, def string) (float64, error) {
	raw := orDefault(key, def)
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number, got %q: %w", key, raw, err)
	}
	if v < 0 || v > 1 {
		return 0, fmt.Errorf("%s must be between 0 and 1, got %q", key, raw)
	}
	return v, nil
}

func parseLogLevel(key, def string) (slog.Level, error) {
	raw := orDefault(key, def)
	var level slog.Level
	if err := level.UnmarshalText([]byte(raw)); err != nil {
		return 0, fmt.Errorf("%s must be a valid slog level, got %q: %w", key, raw, err)
	}
	return level, nil
}

// parseCSV reads a comma-separated string from env (or default), trims
// each token, and drops empty entries. Used for LLMGATE_FALLBACK_ON.
func parseCSV(key, def string) []string {
	raw := orDefault(key, def)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func orDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
