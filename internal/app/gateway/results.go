package gateway

import (
	"context"
	"fmt"
	"log/slog"

	llmresultsink "llmgate/internal/domain/llmresult/sink"
	"llmgate/internal/platform/config"
	natsllmresult "llmgate/internal/platform/nats/llmresult"
)

func buildResultSink(ctx context.Context, cfg *config.Server, log *slog.Logger) (llmresultsink.Sink, error) {
	if cfg == nil || cfg.LLMResultNATSURL == "" {
		return llmresultsink.NopSink{}, nil
	}
	publisher, err := natsllmresult.NewPublisher(ctx, natsllmresult.Config{
		URL:      cfg.LLMResultNATSURL,
		Subject:  cfg.LLMResultNATSSubject,
		User:     cfg.LLMResultNATSUser,
		Password: cfg.LLMResultNATSPassword,
	}, log)
	if err != nil {
		return nil, fmt.Errorf("build llm result nats publisher: %w", err)
	}
	return llmresultsink.NewAsyncSinkWithConfig(publisher, log, llmresultsink.AsyncConfig{ //nolint:contextcheck // AsyncSink worker detaches from request ctx by design (see emitOne)
		QueueSize:     cfg.LLMResultAsyncQueueSize,
		BatchSize:     cfg.LLMResultAsyncBatchSize,
		FlushInterval: cfg.LLMResultAsyncFlush,
		EmitTimeout:   cfg.LLMResultAsyncEmitTimeout,
		CloseTimeout:  cfg.LLMResultAsyncCloseTimeout,
	}), nil
}
