package audit

import (
	"context"
	"log/slog"
)

// SlogRecorder emits each operational Record as one structured slog line. The
// prefix names the backing technology (slog) — same convention as
// bufio.Reader / slog.JSONHandler.
type SlogRecorder struct {
	log *slog.Logger
}

func NewSlogRecorder(log *slog.Logger) *SlogRecorder {
	if log == nil {
		log = slog.Default()
	}
	return &SlogRecorder{log: log}
}

func (r *SlogRecorder) RecordAudit(ctx context.Context, rec *Record) {
	if rec == nil {
		return
	}

	attrs := []slog.Attr{
		slog.Int("schema_version", SchemaVersion),
		slog.String("event_type", EventTypeAudit),
		slog.Time("timestamp", rec.Timestamp),
		slog.String("request_id", rec.RequestID),
		slog.String("operation", rec.Operation),
		slog.Int("status", rec.StatusCode),
		slog.Int64("duration_ms", rec.DurationMS),
	}
	if rec.ConsumerName != "" {
		attrs = append(attrs, slog.String("consumer_name", rec.ConsumerName))
	}
	if rec.ConsumerKeyID != "" {
		attrs = append(attrs, slog.String("consumer_key_id", rec.ConsumerKeyID))
	}
	if rec.AuthError != "" {
		attrs = append(attrs, slog.String("auth_error", string(rec.AuthError)))
	}
	if rec.Kind != "" {
		attrs = append(attrs, slog.String("error_kind", string(rec.Kind)))
	}

	r.log.LogAttrs(ctx, slog.LevelInfo, "audit", attrs...)
}

func (r *SlogRecorder) Close() error { return nil }
