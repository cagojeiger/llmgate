package gateway

import (
	"io"
	"log/slog"
	"testing"

	llmresultsink "llmgate/internal/domain/llmresult/sink"
	"llmgate/internal/platform/config"
)

func TestBuildResultSink_DisabledByDefault(t *testing.T) {
	got, record, err := buildResultSink(&config.Server{}, discardLogger())
	if err != nil {
		t.Fatalf("buildResultSink() error = %v", err)
	}
	if _, ok := got.(llmresultsink.NopSink); !ok {
		t.Fatalf("sink type = %T, want NopSink", got)
	}
	if record != nil {
		t.Fatal("audit is disabled but the recorder is set")
	}
}

func TestBuildResultSink_EnabledIsAsyncWrapped(t *testing.T) {
	cfg := &config.Server{AuditDir: t.TempDir()} // local-only, no S3
	got, record, err := buildResultSink(cfg, discardLogger())
	if err != nil {
		t.Fatalf("buildResultSink() error = %v", err)
	}
	if _, ok := got.(*llmresultsink.AsyncSink); !ok {
		t.Fatalf("sink type = %T, want *AsyncSink", got)
	}
	_ = got.Close()
	if record == nil {
		t.Fatal("audit is enabled but the recorder is missing")
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
