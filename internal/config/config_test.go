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
		"LLMGATE_NATS_URL",
		"LLMGATE_NATS_STREAM",
		"LLMGATE_NATS_SUBJECT",
		"LLMGATE_NATS_QUEUE_SIZE",
		"LLMGATE_NATS_WORKERS",
		"LLMGATE_NATS_BATCH_SIZE",
		"LLMGATE_NATS_BATCH_MAX_WAIT",
		"LLMGATE_NATS_ENQUEUE_TIMEOUT",
		"LLMGATE_NATS_SEND_TIMEOUT",
		"LLMGATE_NATS_FLUSH_TIMEOUT",
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
	if cfg.NATSURL != "" || cfg.NATSStream != "LLMGATE_LLM_RESULTS" || cfg.NATSSubject != "llmgate.llm.results.v1" {
		t.Errorf("NATS identity = url:%q stream:%q subject:%q", cfg.NATSURL, cfg.NATSStream, cfg.NATSSubject)
	}
	if cfg.NATSQueueSize != 10000 || cfg.NATSWorkers != 1 || cfg.NATSBatchSize != 100 {
		t.Errorf("NATS queue/workers/batch = %d/%d/%d, want 10000/1/100", cfg.NATSQueueSize, cfg.NATSWorkers, cfg.NATSBatchSize)
	}
	if cfg.NATSBatchMaxWait != time.Second || cfg.NATSEnqueueTimeout != 100*time.Millisecond || cfg.NATSSendTimeout != 3*time.Second || cfg.NATSFlushTimeout != 15*time.Second {
		t.Errorf("NATS timeouts = batch:%v enqueue:%v send:%v flush:%v", cfg.NATSBatchMaxWait, cfg.NATSEnqueueTimeout, cfg.NATSSendTimeout, cfg.NATSFlushTimeout)
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
