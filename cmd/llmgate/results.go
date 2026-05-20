package main

import (
	"context"
	"fmt"
	"log/slog"

	"llmgate/internal/config"
	llmresultsink "llmgate/internal/events/llmresult/sink"
	natstransport "llmgate/internal/events/llmresult/transport/nats"
)

func buildResultSink(ctx context.Context, cfg *config.Server, log *slog.Logger) (llmresultsink.Sink, error) {
	if cfg == nil || cfg.LLMResultNATSURL == "" {
		return llmresultsink.NopSink{}, nil
	}
	publisher, err := natstransport.NewPublisher(ctx, natstransport.Config{
		URL:     cfg.LLMResultNATSURL,
		Stream:  cfg.LLMResultNATSStream,
		Subject: cfg.LLMResultNATSSubject,
	}, log)
	if err != nil {
		return nil, fmt.Errorf("build llm result nats publisher: %w", err)
	}
	return llmresultsink.NewAsyncSinkWithConfig(publisher, log, llmresultsink.AsyncConfig{
		QueueSize:     cfg.LLMResultAsyncQueueSize,
		BatchSize:     cfg.LLMResultAsyncBatchSize,
		FlushInterval: cfg.LLMResultAsyncFlush,
	}), nil
}
