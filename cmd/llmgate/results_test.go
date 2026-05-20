package main

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"llmgate/internal/config"
	llmresultsink "llmgate/internal/domain/llmresult/sink"
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
