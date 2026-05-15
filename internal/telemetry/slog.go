package telemetry

import (
	"context"
	"log/slog"
)

// SlogAuditRecorder emits each operational AuditEvent as one structured slog line. The
// prefix names the backing technology (slog) — same convention as
// bufio.Reader / slog.JSONHandler.
type SlogAuditRecorder struct {
	log *slog.Logger
}

func NewSlogAuditRecorder(log *slog.Logger) *SlogAuditRecorder {
	if log == nil {
		log = slog.Default()
	}
	return &SlogAuditRecorder{log: log}
}

func (r *SlogAuditRecorder) RecordAudit(ctx context.Context, rec *AuditEvent) {
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

func (r *SlogAuditRecorder) Close() error { return nil }

// SlogCallRecorder emits each CallEvent as one structured slog line.
type SlogCallRecorder struct {
	log *slog.Logger
}

func NewSlogCallRecorder(log *slog.Logger) *SlogCallRecorder {
	if log == nil {
		log = slog.Default()
	}
	return &SlogCallRecorder{log: log}
}

func (r *SlogCallRecorder) RecordCall(ctx context.Context, rec *CallEvent) {
	if rec == nil {
		return
	}

	attrs := []slog.Attr{
		slog.Int("schema_version", SchemaVersion),
		slog.String("event_type", EventTypeCall),
		slog.Time("timestamp", rec.Timestamp),
		slog.String("request_id", rec.RequestID),
		slog.String("operation", rec.Operation),
		slog.String("model_requested", rec.ModelRequested),
		slog.Int("status", rec.StatusCode),
		slog.Int64("duration_ms", rec.DurationMS),
		slog.Int64("request_bytes", rec.RequestBytes),
		slog.Int64("response_bytes", rec.ResponseBytes),
	}
	if rec.ConsumerName != "" {
		attrs = append(attrs, slog.String("consumer_name", rec.ConsumerName))
	}
	if rec.ConsumerKeyID != "" {
		attrs = append(attrs, slog.String("consumer_key_id", rec.ConsumerKeyID))
	}
	if rec.Vendor != "" {
		attrs = append(attrs, slog.String("vendor", rec.Vendor))
	}
	if rec.ModelUsed != "" && rec.ModelUsed != rec.ModelRequested {
		attrs = append(attrs, slog.String("model_used", rec.ModelUsed))
	}
	if rec.Kind != "" {
		attrs = append(attrs, slog.String("error_kind", string(rec.Kind)))
	}
	if rec.Usage != nil {
		attrs = append(attrs,
			slog.Int("prompt_tokens", rec.Usage.PromptTokens),
			slog.Int("completion_tokens", rec.Usage.CompletionTokens),
			slog.Int("total_tokens", rec.Usage.TotalTokens),
		)
	}
	if rec.VendorCost != "" {
		attrs = append(attrs, slog.String("vendor_cost", rec.VendorCost))
	}
	if len(rec.Attempts) > 1 {
		attrs = append(attrs, slog.Any("attempts", rec.Attempts))
	}

	r.log.LogAttrs(ctx, slog.LevelInfo, "call", attrs...)
}

func (r *SlogCallRecorder) Close() error { return nil }
