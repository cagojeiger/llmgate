package main

import (
	"log/slog"

	llmresultsink "llmgate/internal/events/llmresult/sink"
)

func buildResultSink(_ *slog.Logger) llmresultsink.Sink {
	return llmresultsink.NopSink{}
}
