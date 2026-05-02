package config

import (
	"log/slog"
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
		"LLMGATE_SHUTDOWN_HEADER_TIMEOUT",
		"LLMGATE_SHUTDOWN_DRAIN_TIMEOUT",
		"LLMGATE_LOG_LEVEL",
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
	if cfg.ShutdownHeaderTimeout != 3*time.Second {
		t.Errorf("ShutdownHeaderTimeout = %v, want 3s", cfg.ShutdownHeaderTimeout)
	}
	if cfg.ShutdownDrainTimeout != 7*time.Second {
		t.Errorf("ShutdownDrainTimeout = %v, want 7s", cfg.ShutdownDrainTimeout)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", cfg.LogLevel)
	}
}

func TestLoadServer_OverrideFromEnv(t *testing.T) {
	resetEnv(t)
	t.Setenv("LLMGATE_ADDR", "0.0.0.0:9090")
	t.Setenv("LLMGATE_SHUTDOWN_HEADER_TIMEOUT", "5s")
	t.Setenv("LLMGATE_SHUTDOWN_DRAIN_TIMEOUT", "12s")
	t.Setenv("LLMGATE_LOG_LEVEL", "debug")

	cfg, err := LoadServer()
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.Addr != "0.0.0.0:9090" {
		t.Errorf("Addr = %q, want override", cfg.Addr)
	}
	if cfg.ShutdownHeaderTimeout != 5*time.Second {
		t.Errorf("ShutdownHeaderTimeout = %v, want 5s", cfg.ShutdownHeaderTimeout)
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
	t.Setenv("LLMGATE_SHUTDOWN_HEADER_TIMEOUT", "not-a-duration")

	_, err := LoadServer()
	if err == nil {
		t.Fatal("LoadServer: want error for malformed duration")
	}
	if !strings.Contains(err.Error(), "LLMGATE_SHUTDOWN_HEADER_TIMEOUT") {
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
