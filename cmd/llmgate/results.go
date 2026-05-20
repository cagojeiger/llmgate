package main

import (
	"context"
	"fmt"
	"log/slog"

	"llmgate/internal/config"
	llmresultsink "llmgate/internal/domain/llmresult/sink"
	natsllmresult "llmgate/internal/platform/nats/llmresult"
)

func buildResultSink(ctx context.Context, cfg *config.Server, log *slog.Logger) (llmresultsink.Sink, error) {
	if cfg == nil || cfg.LLMResultNATSURL == "" {
		return llmresultsink.NopSink{}, nil
	}
	publisher, err := natsllmresult.NewPublisher(ctx, natsllmresult.Config{
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
