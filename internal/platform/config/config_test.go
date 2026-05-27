package config

import (
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
)

// resetEnv clears every LLMGATE_* env var that LoadServer reads, so a
// test starts from a known baseline regardless of the developer shell
// or other tests in the package. t.Setenv automatically restores the
// prior value at end of test.
func resetEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"LLMGATE_ADDR",
		"LLMGATE_ENVIRONMENT",
		"LLMGATE_SHUTDOWN_DRAIN_TIMEOUT",
		"LLMGATE_LOG_LEVEL",
		"LLMGATE_FALLBACK_ON",
		"LLMGATE_CIRCUIT_FAILURES",
		"LLMGATE_CIRCUIT_OPEN_DURATION",
		"LLMGATE_CIRCUIT_MAX_OPEN_DURATION",
		"LLMGATE_CIRCUIT_JITTER",
		"LLMGATE_REQUEST_TIMEOUT",
		"LLMGATE_COMPLETE_TIMEOUT",
		"LLMGATE_STREAM_IDLE_TIMEOUT",
		"LLMGATE_METRICS_ENABLED",
		"LLMGATE_LLMRESULT_NATS_URL",
		"LLMGATE_LLMRESULT_NATS_SUBJECT",
		"LLMGATE_LLMRESULT_NATS_USER",
		"LLMGATE_LLMRESULT_NATS_PASSWORD",
		"LLMGATE_LLMRESULT_ASYNC_QUEUE_SIZE",
		"LLMGATE_LLMRESULT_ASYNC_BATCH_SIZE",
		"LLMGATE_LLMRESULT_ASYNC_FLUSH_INTERVAL",
		"LLMGATE_LLMRESULT_ASYNC_EMIT_TIMEOUT",
		"LLMGATE_LLMRESULT_ASYNC_CLOSE_TIMEOUT",
	} {
		t.Setenv(k, "")
	}
}

func TestLoadServer_Defaults(t *testing.T) {
	resetEnv(t)

	cfg, err := LoadServer()
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.Addr != ":8080" {
		t.Errorf("Addr = %q, want :8080", cfg.Addr)
	}
	if cfg.Environment != "local" {
		t.Errorf("Environment = %q, want local", cfg.Environment)
	}
	if cfg.ShutdownDrainTimeout != 5*time.Minute {
		t.Errorf("ShutdownDrainTimeout = %v, want 5m", cfg.ShutdownDrainTimeout)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", cfg.LogLevel)
	}
	if !reflect.DeepEqual(cfg.FallbackOn, []string{"rate_limit", "upstream", "timeout", "network"}) {
		t.Errorf("FallbackOn = %v, want default transient classes", cfg.FallbackOn)
	}
	if cfg.CircuitFailures != 3 {
		t.Errorf("CircuitFailures = %d, want 3", cfg.CircuitFailures)
	}
	if cfg.CircuitOpen != 30*time.Second {
		t.Errorf("CircuitOpen = %v, want 30s", cfg.CircuitOpen)
	}
	if cfg.CircuitMaxOpen != 5*time.Minute {
		t.Errorf("CircuitMaxOpen = %v, want 5m", cfg.CircuitMaxOpen)
	}
	if cfg.CircuitJitter != 0.2 {
		t.Errorf("CircuitJitter = %v, want 0.2", cfg.CircuitJitter)
	}
	if cfg.RequestTimeout != 5*time.Minute {
		t.Errorf("RequestTimeout = %v, want 5m", cfg.RequestTimeout)
	}
	if cfg.CompleteTimeout != time.Minute {
		t.Errorf("CompleteTimeout = %v, want 1m", cfg.CompleteTimeout)
	}
	if cfg.StreamIdleTimeout != time.Minute {
		t.Errorf("StreamIdleTimeout = %v, want 1m", cfg.StreamIdleTimeout)
	}
	if cfg.MetricsEnabled {
		t.Error("MetricsEnabled = true, want false default")
	}
	if cfg.LLMResultNATSURL != "" {
		t.Errorf("LLMResultNATSURL = %q, want disabled empty default", cfg.LLMResultNATSURL)
	}
	if cfg.LLMResultNATSSubject != "llmgate.llmresult.finalized" {
		t.Errorf("LLMResultNATSSubject = %q, want llmgate.llmresult.finalized", cfg.LLMResultNATSSubject)
	}
	if cfg.LLMResultNATSUser != "" {
		t.Errorf("LLMResultNATSUser = %q, want empty (anonymous)", cfg.LLMResultNATSUser)
	}
	if cfg.LLMResultNATSPassword != "" {
		t.Errorf("LLMResultNATSPassword = %q, want empty", cfg.LLMResultNATSPassword)
	}
	if cfg.LLMResultAsyncQueueSize != 1000 {
		t.Errorf("LLMResultAsyncQueueSize = %d, want 1000", cfg.LLMResultAsyncQueueSize)
	}
	if cfg.LLMResultAsyncBatchSize != 100 {
		t.Errorf("LLMResultAsyncBatchSize = %d, want 100", cfg.LLMResultAsyncBatchSize)
	}
	if cfg.LLMResultAsyncFlush != time.Second {
		t.Errorf("LLMResultAsyncFlush = %v, want 1s", cfg.LLMResultAsyncFlush)
	}
}

func TestLoadServer_FallbackOnOverride(t *testing.T) {
	resetEnv(t)
	t.Setenv("LLMGATE_FALLBACK_ON", " rate_limit, upstream , empty_response ")

	cfg, err := LoadServer()
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	want := []string{"rate_limit", "upstream", "empty_response"}
	if !reflect.DeepEqual(cfg.FallbackOn, want) {
		t.Errorf("FallbackOn = %v, want %v (trimmed)", cfg.FallbackOn, want)
	}
}

func TestLoadServer_CircuitDisabledByZero(t *testing.T) {
	// Operators turn the breaker off by setting failures or duration to 0.
	resetEnv(t)
	t.Setenv("LLMGATE_CIRCUIT_FAILURES", "0")
	t.Setenv("LLMGATE_CIRCUIT_OPEN_DURATION", "0s")

	cfg, err := LoadServer()
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.CircuitFailures != 0 {
		t.Errorf("CircuitFailures = %d, want 0 (explicitly disabled)", cfg.CircuitFailures)
	}
	if cfg.CircuitOpen != 0 {
		t.Errorf("CircuitOpen = %v, want 0 (explicitly disabled)", cfg.CircuitOpen)
	}
}

func TestLoadServer_RejectsNegativeCircuit(t *testing.T) {
	resetEnv(t)
	t.Setenv("LLMGATE_CIRCUIT_FAILURES", "-1")

	_, err := LoadServer()
	if err == nil {
		t.Fatal("LoadServer: want error for negative LLMGATE_CIRCUIT_FAILURES")
	}
	if !strings.Contains(err.Error(), "LLMGATE_CIRCUIT_FAILURES") {
		t.Errorf("err = %v, want mention of failing key", err)
	}
}

func TestLoadServer_CircuitBackoffOverrides(t *testing.T) {
	resetEnv(t)
	t.Setenv("LLMGATE_CIRCUIT_OPEN_DURATION", "10s")
	t.Setenv("LLMGATE_CIRCUIT_MAX_OPEN_DURATION", "2m")
	t.Setenv("LLMGATE_CIRCUIT_JITTER", "0.35")

	cfg, err := LoadServer()
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.CircuitOpen != 10*time.Second {
		t.Errorf("CircuitOpen = %v, want 10s", cfg.CircuitOpen)
	}
	if cfg.CircuitMaxOpen != 2*time.Minute {
		t.Errorf("CircuitMaxOpen = %v, want 2m", cfg.CircuitMaxOpen)
	}
	if cfg.CircuitJitter != 0.35 {
		t.Errorf("CircuitJitter = %v, want 0.35", cfg.CircuitJitter)
	}
}

func TestLoadServer_CompleteTimeoutOverrides(t *testing.T) {
	resetEnv(t)
	t.Setenv("LLMGATE_REQUEST_TIMEOUT", "45s")
	t.Setenv("LLMGATE_COMPLETE_TIMEOUT", "10s")

	cfg, err := LoadServer()
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.RequestTimeout != 45*time.Second {
		t.Errorf("RequestTimeout = %v, want 45s", cfg.RequestTimeout)
	}
	if cfg.CompleteTimeout != 10*time.Second {
		t.Errorf("CompleteTimeout = %v, want 10s", cfg.CompleteTimeout)
	}
}

func TestLoadServer_StreamIdleTimeoutOverride(t *testing.T) {
	resetEnv(t)
	t.Setenv("LLMGATE_STREAM_IDLE_TIMEOUT", "20s")

	cfg, err := LoadServer()
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.StreamIdleTimeout != 20*time.Second {
		t.Errorf("StreamIdleTimeout = %v, want 20s", cfg.StreamIdleTimeout)
	}
}

func TestLoadServer_LLMResultNATSOverrides(t *testing.T) {
	resetEnv(t)
	t.Setenv("LLMGATE_LLMRESULT_NATS_URL", "nats://localhost:4222")
	t.Setenv("LLMGATE_LLMRESULT_NATS_SUBJECT", "results.finalized")
	t.Setenv("LLMGATE_LLMRESULT_NATS_USER", "llmgate")
	t.Setenv("LLMGATE_LLMRESULT_NATS_PASSWORD", "s3cret")
	t.Setenv("LLMGATE_LLMRESULT_ASYNC_QUEUE_SIZE", "25")
	t.Setenv("LLMGATE_LLMRESULT_ASYNC_BATCH_SIZE", "5")
	t.Setenv("LLMGATE_LLMRESULT_ASYNC_FLUSH_INTERVAL", "250ms")

	cfg, err := LoadServer()
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.LLMResultNATSURL != "nats://localhost:4222" {
		t.Errorf("LLMResultNATSURL = %q", cfg.LLMResultNATSURL)
	}
	if cfg.LLMResultNATSSubject != "results.finalized" {
		t.Errorf("LLMResultNATSSubject = %q", cfg.LLMResultNATSSubject)
	}
	if cfg.LLMResultNATSUser != "llmgate" {
		t.Errorf("LLMResultNATSUser = %q, want llmgate", cfg.LLMResultNATSUser)
	}
	if cfg.LLMResultNATSPassword != "s3cret" {
		t.Errorf("LLMResultNATSPassword = %q, want s3cret", cfg.LLMResultNATSPassword)
	}
	if cfg.LLMResultAsyncQueueSize != 25 {
		t.Errorf("LLMResultAsyncQueueSize = %d, want 25", cfg.LLMResultAsyncQueueSize)
	}
	if cfg.LLMResultAsyncBatchSize != 5 {
		t.Errorf("LLMResultAsyncBatchSize = %d, want 5", cfg.LLMResultAsyncBatchSize)
	}
	if cfg.LLMResultAsyncFlush != 250*time.Millisecond {
		t.Errorf("LLMResultAsyncFlush = %v, want 250ms", cfg.LLMResultAsyncFlush)
	}
}

func TestLoadServer_RejectsNegativeLLMResultQueueSize(t *testing.T) {
	resetEnv(t)
	t.Setenv("LLMGATE_LLMRESULT_ASYNC_QUEUE_SIZE", "-1")

	_, err := LoadServer()
	if err == nil {
		t.Fatal("LoadServer: want error for negative LLMGATE_LLMRESULT_ASYNC_QUEUE_SIZE")
	}
	if !strings.Contains(err.Error(), "LLMGATE_LLMRESULT_ASYNC_QUEUE_SIZE") {
		t.Errorf("err = %v, want mention of failing key", err)
	}
}

func TestLoadServer_RejectsNegativeLLMResultBatchSize(t *testing.T) {
	resetEnv(t)
	t.Setenv("LLMGATE_LLMRESULT_ASYNC_BATCH_SIZE", "-1")

	_, err := LoadServer()
	if err == nil {
		t.Fatal("LoadServer: want error for negative LLMGATE_LLMRESULT_ASYNC_BATCH_SIZE")
	}
	if !strings.Contains(err.Error(), "LLMGATE_LLMRESULT_ASYNC_BATCH_SIZE") {
		t.Errorf("err = %v, want mention of failing key", err)
	}
}

func TestLoadServer_RejectsNegativeLLMResultFlushInterval(t *testing.T) {
	resetEnv(t)
	t.Setenv("LLMGATE_LLMRESULT_ASYNC_FLUSH_INTERVAL", "-1s")

	_, err := LoadServer()
	if err == nil {
		t.Fatal("LoadServer: want error for negative LLMGATE_LLMRESULT_ASYNC_FLUSH_INTERVAL")
	}
	if !strings.Contains(err.Error(), "LLMGATE_LLMRESULT_ASYNC_FLUSH_INTERVAL") {
		t.Errorf("err = %v, want mention of failing key", err)
	}
}

func TestLoadServer_MetricsEnabledOverride(t *testing.T) {
	resetEnv(t)
	t.Setenv("LLMGATE_METRICS_ENABLED", "true")

	cfg, err := LoadServer()
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if !cfg.MetricsEnabled {
		t.Fatal("MetricsEnabled = false, want true")
	}
}

func TestLoadServer_RejectsInvalidMetricsEnabled(t *testing.T) {
	resetEnv(t)
	t.Setenv("LLMGATE_METRICS_ENABLED", "sometimes")

	_, err := LoadServer()
	if err == nil {
		t.Fatal("LoadServer: want error for invalid LLMGATE_METRICS_ENABLED")
	}
	if !strings.Contains(err.Error(), "LLMGATE_METRICS_ENABLED") {
		t.Errorf("err = %v, want metrics enabled key", err)
	}
}

func TestLoadServer_RequiresSecureNATSOutsideLocal(t *testing.T) {
	resetEnv(t)
	t.Setenv("LLMGATE_ENVIRONMENT", "prod")
	t.Setenv("LLMGATE_LLMRESULT_NATS_URL", "nats://nats:4222")
	t.Setenv("LLMGATE_LLMRESULT_NATS_USER", "llmgate")
	t.Setenv("LLMGATE_LLMRESULT_NATS_PASSWORD", "s3cret")

	_, err := LoadServer()
	if err == nil {
		t.Fatal("LoadServer: want error for plaintext NATS outside local")
	}
	if !strings.Contains(err.Error(), "tls://") {
		t.Errorf("err = %v, want tls scheme requirement", err)
	}
}

func TestLoadServer_AllowsSecureNATSOutsideLocal(t *testing.T) {
	resetEnv(t)
	t.Setenv("LLMGATE_ENVIRONMENT", "prod")
	t.Setenv("LLMGATE_LLMRESULT_NATS_URL", "tls://nats:4222")
	t.Setenv("LLMGATE_LLMRESULT_NATS_USER", "llmgate")
	t.Setenv("LLMGATE_LLMRESULT_NATS_PASSWORD", "s3cret")

	cfg, err := LoadServer()
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.LLMResultNATSURL != "tls://nats:4222" {
		t.Errorf("LLMResultNATSURL = %q", cfg.LLMResultNATSURL)
	}
}

func TestLoadServer_RejectsInvalidCircuitJitter(t *testing.T) {
	resetEnv(t)
	t.Setenv("LLMGATE_CIRCUIT_JITTER", "1.5")

	_, err := LoadServer()
	if err == nil {
		t.Fatal("LoadServer: want error for invalid jitter")
	}
	if !strings.Contains(err.Error(), "LLMGATE_CIRCUIT_JITTER") {
		t.Errorf("err = %v, want mention of failing key", err)
	}
}

func TestLoadServer_OverrideFromEnv(t *testing.T) {
	resetEnv(t)
	t.Setenv("LLMGATE_ADDR", "0.0.0.0:9090")
	t.Setenv("LLMGATE_ENVIRONMENT", "prod")
	t.Setenv("LLMGATE_SHUTDOWN_DRAIN_TIMEOUT", "12s")
	t.Setenv("LLMGATE_LOG_LEVEL", "debug")
	t.Setenv("LLMGATE_METRICS_ENABLED", "true")

	cfg, err := LoadServer()
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.Addr != "0.0.0.0:9090" {
		t.Errorf("Addr = %q, want override", cfg.Addr)
	}
	if cfg.Environment != "prod" {
		t.Errorf("Environment = %q, want prod", cfg.Environment)
	}
	if cfg.ShutdownDrainTimeout != 12*time.Second {
		t.Errorf("ShutdownDrainTimeout = %v, want 12s", cfg.ShutdownDrainTimeout)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v, want debug", cfg.LogLevel)
	}
	if !cfg.MetricsEnabled {
		t.Error("MetricsEnabled = false, want override")
	}
}

func TestLoadServer_RejectsMalformedDuration(t *testing.T) {
	resetEnv(t)
	t.Setenv("LLMGATE_SHUTDOWN_DRAIN_TIMEOUT", "not-a-duration")

	_, err := LoadServer()
	if err == nil {
		t.Fatal("LoadServer: want error for malformed duration")
	}
	if !strings.Contains(err.Error(), "LLMGATE_SHUTDOWN_DRAIN_TIMEOUT") {
		t.Errorf("err = %v, want mention of the failing key", err)
	}
}

func TestLoadServer_RejectsNonPositiveDuration(t *testing.T) {
	resetEnv(t)
	t.Setenv("LLMGATE_SHUTDOWN_DRAIN_TIMEOUT", "0s")

	_, err := LoadServer()
	if err == nil {
		t.Fatal("LoadServer: want error for non-positive duration")
	}
	if !strings.Contains(err.Error(), "LLMGATE_SHUTDOWN_DRAIN_TIMEOUT") {
		t.Errorf("err = %v, want mention of the failing key", err)
	}
}

func TestLoadServer_RejectsUnknownLogLevel(t *testing.T) {
	resetEnv(t)
	t.Setenv("LLMGATE_LOG_LEVEL", "verbose")

	_, err := LoadServer()
	if err == nil {
		t.Fatal("LoadServer: want error for unknown level")
	}
	if !strings.Contains(err.Error(), "LLMGATE_LOG_LEVEL") {
		t.Errorf("err = %v, want mention of the failing key", err)
	}
}
