package config

import (
	"log/slog"
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
