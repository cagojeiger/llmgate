package gateway

import (
	"fmt"
	"log/slog"

	llmresultsink "llmgate/internal/domain/llmresult/sink"
	"llmgate/internal/platform/audit"
	"llmgate/internal/platform/config"
)

// buildResultSink builds the finalized-result sink: the audit file sink
// wrapped in the shared AsyncSink so writes stay off the request path.
// A nil cfg or empty AuditDir disables it (NopSink).
func buildResultSink(cfg *config.Server, log *slog.Logger) (llmresultsink.Sink, error) {
	if cfg == nil || cfg.AuditDir == "" {
		return llmresultsink.NopSink{}, nil
	}
	terminal, err := buildAuditTerminal(cfg, log)
	if err != nil {
		return nil, err
	}
	return wrapAsync(terminal, cfg, log), nil
}

func buildAuditTerminal(cfg *config.Server, log *slog.Logger) (llmresultsink.Sink, error) {
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
		Dir:               cfg.AuditDir,
		RotateInterval:    cfg.AuditRotateInterval,
		RotateMaxBytes:    cfg.AuditRotateMaxBytes,
		UploadInterval:    cfg.AuditUploadInterval,
		Retention:         cfg.AuditRetention,
		DiskCap:           cfg.AuditDiskCap,
		Compression:       cfg.AuditCompression,
		UploadConcurrency: cfg.AuditUploadConcurrency,
	}, store, cfg.AuditS3Prefix, log)
	if err != nil {
		return nil, fmt.Errorf("build audit file sink: %w", err)
	}
	target := cfg.AuditS3Endpoint
	if target == "" {
		target = "(local-only, no upload)"
	}
	// Startup visibility: an operator should be able to tell from the logs
	// that the sink is on and how it is tuned. Credentials are never
	// logged.
	log.Info("audit result sink enabled",
		slog.String("dir", cfg.AuditDir),
		slog.String("s3_endpoint", target),
		slog.String("s3_bucket", cfg.AuditS3Bucket),
		// human-readable durations ("1h0m0s"), not raw nanoseconds, so the
		// startup line reads cleanly in log analysis.
		slog.String("rotate_interval", cfg.AuditRotateInterval.String()),
		slog.String("upload_interval", cfg.AuditUploadInterval.String()),
		slog.String("retention", cfg.AuditRetention.String()),
		slog.String("compression", cfg.AuditCompression),
		slog.Int("upload_concurrency", cfg.AuditUploadConcurrency))
	return fileSink, nil
}

// wrapAsync keeps transport backpressure off the request path: the audit
// FileSink and the AsyncSink worker both detach from the build ctx by
// design and stop on Close.
//
//nolint:contextcheck // detached background workers by design (see AsyncSink.emitOne, FileSink goroutines)
func wrapAsync(terminal llmresultsink.Sink, cfg *config.Server, log *slog.Logger) llmresultsink.Sink {
	return llmresultsink.NewAsyncSinkWithConfig(terminal, log, llmresultsink.AsyncConfig{
		QueueSize:     cfg.LLMResultAsyncQueueSize,
		BatchSize:     cfg.LLMResultAsyncBatchSize,
		FlushInterval: cfg.LLMResultAsyncFlush,
		EmitTimeout:   cfg.LLMResultAsyncEmitTimeout,
		CloseTimeout:  cfg.LLMResultAsyncCloseTimeout,
	})
}
