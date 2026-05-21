package gateway

import (
	"context"
	"io"
	"log/slog"
	"testing"

	llmresultsink "llmgate/internal/domain/llmresult/sink"
	"llmgate/internal/platform/config"
)

func TestBuildResultSink_DisabledByDefault(t *testing.T) {
	got, err := buildResultSink(context.Background(), &config.Server{}, discardLogger())
	if err != nil {
		t.Fatalf("buildResultSink() error = %v", err)
	}
	if _, ok := got.(llmresultsink.NopSink); !ok {
		t.Fatalf("sink type = %T, want NopSink", got)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
