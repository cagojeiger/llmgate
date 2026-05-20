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
	Addr string
	// Environment labels logs and future telemetry events with the
	// deployment boundary operators search by (local, staging, prod).
	Environment string
	// ShutdownDrainTimeout caps how long graceful shutdown waits for
	// in-flight requests to finish before force-closing any survivors.
	// Default 5m comfortably covers typical LLM streams; the
	// orchestrator's terminationGracePeriodSeconds (k8s) /
	// stop_grace_period (compose) should be set slightly larger so the
	// app-side force close fires before SIGKILL.
	ShutdownDrainTimeout time.Duration
	LogLevel             slog.Level

	// Routing fallback, breaker, and timeout settings.
	FallbackOn        []string
	CircuitFailures   int
	CircuitOpen       time.Duration
	CircuitMaxOpen    time.Duration
	CircuitJitter     float64
	RequestTimeout    time.Duration
	CompleteTimeout   time.Duration
	StreamIdleTimeout time.Duration

	// Finalized LLM result event publishing. Empty NATS URL disables remote
	// publishing; the server still builds result events and drops them through
	// the no-op sink.
	LLMResultNATSURL        string
	LLMResultNATSStream     string
	LLMResultNATSSubject    string
	LLMResultAsyncQueueSize int
	LLMResultAsyncBatchSize int
	LLMResultAsyncFlush     time.Duration
}

func LoadServer() (*Server, error) {
	drainTimeout, err := positiveDuration("LLMGATE_SHUTDOWN_DRAIN_TIMEOUT", "5m")
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
	requestTimeout, err := nonNegativeDuration("LLMGATE_REQUEST_TIMEOUT", "5m")
	if err != nil {
		return nil, err
	}
	completeTimeout, err := nonNegativeDuration("LLMGATE_COMPLETE_TIMEOUT", "1m")
	if err != nil {
		return nil, err
	}
	streamIdleTimeout, err := nonNegativeDuration("LLMGATE_STREAM_IDLE_TIMEOUT", "1m")
	if err != nil {
		return nil, err
	}
	llmResultQueueSize, err := nonNegativeInt("LLMGATE_LLMRESULT_ASYNC_QUEUE_SIZE", "1000")
	if err != nil {
		return nil, err
	}
	llmResultBatchSize, err := nonNegativeInt("LLMGATE_LLMRESULT_ASYNC_BATCH_SIZE", "100")
	if err != nil {
		return nil, err
	}
	llmResultFlush, err := nonNegativeDuration("LLMGATE_LLMRESULT_ASYNC_FLUSH_INTERVAL", "1s")
	if err != nil {
		return nil, err
	}

	return &Server{
		Addr:                    orDefault("LLMGATE_ADDR", ":8080"),
		Environment:             orDefault("LLMGATE_ENVIRONMENT", "local"),
		ShutdownDrainTimeout:    drainTimeout,
		LogLevel:                logLevel,
		FallbackOn:              parseCSV("LLMGATE_FALLBACK_ON", "rate_limit,upstream,timeout,network"),
		CircuitFailures:         circuitFailures,
		CircuitOpen:             circuitOpen,
		CircuitMaxOpen:          circuitMaxOpen,
		CircuitJitter:           circuitJitter,
		RequestTimeout:          requestTimeout,
		CompleteTimeout:         completeTimeout,
		StreamIdleTimeout:       streamIdleTimeout,
		LLMResultNATSURL:        orDefault("LLMGATE_LLMRESULT_NATS_URL", ""),
		LLMResultNATSStream:     orDefault("LLMGATE_LLMRESULT_NATS_STREAM", "LLMRESULT"),
		LLMResultNATSSubject:    orDefault("LLMGATE_LLMRESULT_NATS_SUBJECT", "llmgate.llmresult.finalized"),
		LLMResultAsyncQueueSize: llmResultQueueSize,
		LLMResultAsyncBatchSize: llmResultBatchSize,
		LLMResultAsyncFlush:     llmResultFlush,
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

// nonNegativeDuration accepts 0 for settings that can be disabled.
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

// nonNegativeInt accepts 0 for settings that can be disabled.
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

// parseCSV reads a comma-separated env value, trims tokens, and drops blanks.
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
