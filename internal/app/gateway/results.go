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

// buildResultSink selects the finalized-result sink. The audit
// (local-rotate + S3) sink takes priority when configured; otherwise the
// NATS publisher; otherwise a no-op. Whichever terminal sink is chosen is
// wrapped by the shared AsyncSink so transport backpressure stays off the
// request path.
func buildResultSink(ctx context.Context, cfg *config.Server, log *slog.Logger) (llmresultsink.Sink, error) {
	switch {
	case cfg == nil:
		return llmresultsink.NopSink{}, nil
	case cfg.AuditDir != "":
		return buildAuditSink(cfg, log) //nolint:contextcheck // FileSink's rotator/shipper goroutines detach from the build ctx by design (they stop on Close, not on build completion)
	case cfg.LLMResultNATSURL != "":
		return buildNATSSink(ctx, cfg, log)
	default:
		return llmresultsink.NopSink{}, nil
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
		})
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
