package main

import (
	"context"
	"fmt"
	"log/slog"

	"llmgate/internal/config"
	llmresultsink "llmgate/internal/events/llmresult/sink"
	"llmgate/internal/events/llmresult/transport"
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
	})
	if err != nil {
		return nil, fmt.Errorf("build llm result nats publisher: %w", err)
	}
	transportSink := transport.NewSink(publisher, transport.JSONEncoder{}, log)
	return llmresultsink.NewAsyncSink(transportSink, log, cfg.LLMResultAsyncQueueSize), nil
}
