package gateway

import (
	"context"
	"fmt"
	"log/slog"

	llmresultsink "llmgate/internal/domain/llmresult/sink"
	"llmgate/internal/platform/audit"
	"llmgate/internal/platform/config"
	natsllmresult "llmgate/internal/platform/nats/llmresult"
)

// buildResultSink assembles the finalized-result sink. The audit
// (local-rotate + S3) and NATS sinks are independent and coexist: each is
// enabled by its own config, and when both are set events fan out to both
// so existing NATS consumers keep working while audit is rolled out. Each
// terminal sink is wrapped by the shared AsyncSink so transport
// backpressure stays off the request path.
func buildResultSink(ctx context.Context, cfg *config.Server, log *slog.Logger) (llmresultsink.Sink, error) {
	if cfg == nil {
		return llmresultsink.NopSink{}, nil
	}
	var sinks []llmresultsink.Sink
	if cfg.AuditDir != "" {
		s, err := buildAuditSink(cfg, log) //nolint:contextcheck // FileSink's rotator/shipper goroutines detach from the build ctx by design (they stop on Close, not on build completion)
		if err != nil {
			return nil, err
		}
		sinks = append(sinks, s)
	}
	if cfg.LLMResultNATSURL != "" {
		s, err := buildNATSSink(ctx, cfg, log)
		if err != nil {
			return nil, err
		}
		sinks = append(sinks, s)
	}
	switch len(sinks) {
	case 0:
		return llmresultsink.NopSink{}, nil
	case 1:
		return sinks[0], nil
	default:
		return llmresultsink.NewFanoutSink(log, sinks...), nil
	}
}

func buildAuditSink(cfg *config.Server, log *slog.Logger) (llmresultsink.Sink, error) {
	var store audit.ObjectStore
	if cfg.AuditS3Endpoint != "" {
		s, err := audit.NewS3Store(audit.S3Config{
			Endpoint:  cfg.AuditS3Endpoint,
			Bucket:    cfg.AuditS3Bucket,
			Region:    cfg.AuditS3Region,
			AccessKey: cfg.AuditS3AccessKey,
			SecretKey: cfg.AuditS3SecretKey,
			UseSSL:    cfg.AuditS3UseSSL,
			PathStyle: cfg.AuditS3PathStyle,
		}, log)
		if err != nil {
			return nil, fmt.Errorf("build audit s3 store: %w", err)
		}
		store = s
	}
	fileSink, err := audit.NewFileSink(audit.Config{
		Dir:            cfg.AuditDir,
		RotateInterval: cfg.AuditRotateInterval,
		RotateMaxBytes: cfg.AuditRotateMaxBytes,
		UploadInterval: cfg.AuditUploadInterval,
		Retention:      cfg.AuditRetention,
		DiskCap:        cfg.AuditDiskCap,
	}, store, cfg.AuditS3Prefix, log)
	if err != nil {
		return nil, fmt.Errorf("build audit file sink: %w", err)
	}
	return wrapAsync(fileSink, cfg, log), nil
}

func buildNATSSink(ctx context.Context, cfg *config.Server, log *slog.Logger) (llmresultsink.Sink, error) {
	publisher, err := natsllmresult.NewPublisher(ctx, natsllmresult.Config{
		URL:      cfg.LLMResultNATSURL,
		Subject:  cfg.LLMResultNATSSubject,
		User:     cfg.LLMResultNATSUser,
		Password: cfg.LLMResultNATSPassword,
	}, log)
	if err != nil {
		return nil, fmt.Errorf("build llm result nats publisher: %w", err)
	}
	return wrapAsync(publisher, cfg, log), nil //nolint:contextcheck // AsyncSink worker detaches from request ctx by design (see emitOne)
}

func wrapAsync(terminal llmresultsink.Sink, cfg *config.Server, log *slog.Logger) llmresultsink.Sink {
	return llmresultsink.NewAsyncSinkWithConfig(terminal, log, llmresultsink.AsyncConfig{

		QueueSize:     cfg.LLMResultAsyncQueueSize,
		BatchSize:     cfg.LLMResultAsyncBatchSize,
		FlushInterval: cfg.LLMResultAsyncFlush,
		EmitTimeout:   cfg.LLMResultAsyncEmitTimeout,
		CloseTimeout:  cfg.LLMResultAsyncCloseTimeout,
	})
}
