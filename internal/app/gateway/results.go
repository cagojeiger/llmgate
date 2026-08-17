package gateway

import (
	"context"
	"fmt"
	"log/slog"

	llmresultsink "llmgate/internal/domain/llmresult/sink"
	"llmgate/internal/platform/audit"
	"llmgate/internal/platform/config"
)

// resultSinkFactory builds one terminal result sink when its config gate
// is set. Registration is an explicit list (defaultResultSinkFactories),
// mirroring defaultProviderFactories rather than init() self-registration:
// the active set stays obvious and a test can inject its own factories.
// build returns the raw terminal — the assembler owns the uniform
// AsyncSink wrapping and the fan-out.
type resultSinkFactory struct {
	name    string
	enabled func(*config.Server) bool
	build   func(context.Context, *config.Server, *slog.Logger) (llmresultsink.Sink, error)
}

func defaultResultSinkFactories() []resultSinkFactory {
	return []resultSinkFactory{
		{
			name:    "audit",
			enabled: func(c *config.Server) bool { return c.AuditDir != "" },
			build:   buildAuditTerminal,
		},
	}
}

// buildResultSink assembles the finalized-result sink from the default
// registry. The async boundary is applied once, at the edge: one queue +
// one worker drains events off the request path, then fans them out
// synchronously to every enabled terminal. This keeps a single buffer of
// body-carrying events (no per-sink duplication, no cross-queue pointer
// pinning) at the cost of the terminals sharing one worker — acceptable
// for best-effort data, and terminals are ordered fast-first (local
// terminals before any remote ones).
func buildResultSink(ctx context.Context, cfg *config.Server, log *slog.Logger) (llmresultsink.Sink, error) {
	return assembleResultSink(ctx, cfg, log, defaultResultSinkFactories())
}

func assembleResultSink(ctx context.Context, cfg *config.Server, log *slog.Logger, factories []resultSinkFactory) (llmresultsink.Sink, error) {
	if cfg == nil {
		return llmresultsink.NopSink{}, nil
	}
	var terminals []llmresultsink.Sink
	for _, f := range factories {
		if !f.enabled(cfg) {
			continue
		}
		terminal, err := f.build(ctx, cfg, log)
		if err != nil {
			return nil, fmt.Errorf("build %s result sink: %w", f.name, err)
		}
		terminals = append(terminals, terminal)
	}
	switch len(terminals) {
	case 0:
		return llmresultsink.NopSink{}, nil
	case 1:
		//nolint:contextcheck // AsyncSink worker detaches from the request/build ctx by design (see emitOne)
		return wrapAsync(terminals[0], cfg, log), nil
	default:
		//nolint:contextcheck // AsyncSink worker detaches from the request/build ctx by design (see emitOne)
		return wrapAsync(llmresultsink.NewFanoutSink(log, terminals...), cfg, log), nil
	}
}

// buildAuditTerminal builds the raw audit FileSink (no async wrap). It
// ignores ctx: the sink's rotator/shipper goroutines detach from the
// build ctx by design and stop on Close.
func buildAuditTerminal(_ context.Context, cfg *config.Server, log *slog.Logger) (llmresultsink.Sink, error) {
	var store audit.ObjectStore
	if cfg.AuditS3Endpoint != "" {
		s, err := audit.NewS3Store(audit.S3Config{ //nolint:contextcheck // NewS3Store uses its own bounded ctx for the one-shot bucket probe
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
	fileSink, err := audit.NewFileSink(audit.Config{ //nolint:contextcheck // FileSink's rotator/shipper goroutines detach from the build ctx by design (stop on Close)
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

func wrapAsync(terminal llmresultsink.Sink, cfg *config.Server, log *slog.Logger) llmresultsink.Sink {
	return llmresultsink.NewAsyncSinkWithConfig(terminal, log, llmresultsink.AsyncConfig{
		QueueSize:     cfg.LLMResultAsyncQueueSize,
		BatchSize:     cfg.LLMResultAsyncBatchSize,
		FlushInterval: cfg.LLMResultAsyncFlush,
		EmitTimeout:   cfg.LLMResultAsyncEmitTimeout,
		CloseTimeout:  cfg.LLMResultAsyncCloseTimeout,
	})
}
