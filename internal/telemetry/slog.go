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

	attrs := commonAttrs(EventTypeAudit, rec.EventCommon)
	if rec.ConsumerName != "" {
		attrs = append(attrs, slog.String("consumer_name", rec.ConsumerName))
	}
	if rec.ConsumerKeyID != "" {
		attrs = append(attrs, slog.String("consumer_key_id", rec.ConsumerKeyID))
	}
	if rec.AuthError != "" {
		attrs = append(attrs, slog.String("auth_error", string(rec.AuthError)))
	}
	if rec.AuthResult != "" {
		attrs = append(attrs, slog.String("auth_result", string(rec.AuthResult)))
	}
	if rec.PolicyResult != "" {
		attrs = append(attrs, slog.String("policy_result", string(rec.PolicyResult)))
	}
	if rec.DenyReason != "" {
		attrs = append(attrs, slog.String("deny_reason", string(rec.DenyReason)))
	}
	if rec.ResourceType != "" {
		attrs = append(attrs, slog.String("resource_type", rec.ResourceType))
	}
	if rec.ResourceID != "" {
		attrs = append(attrs, slog.String("resource_id", rec.ResourceID))
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

	attrs := append(commonAttrs(EventTypeCall, rec.EventCommon),
		slog.String("model_requested", rec.ModelRequested),
		slog.Int64("request_bytes", rec.RequestBytes),
		slog.Int64("response_bytes", rec.ResponseBytes),
	)
	if rec.ConsumerName != "" {
		attrs = append(attrs, slog.String("consumer_name", rec.ConsumerName))
	}
	if rec.ConsumerKeyID != "" {
		attrs = append(attrs, slog.String("consumer_key_id", rec.ConsumerKeyID))
	}
	if rec.Vendor != "" {
		attrs = append(attrs, slog.String("vendor", rec.Vendor))
	}
	attrs = append(attrs, slog.Int("attempts_count", AttemptsCount(rec)))
	if final, ok := FinalAttempt(rec); ok {
		if final.Vendor != "" {
			attrs = append(attrs, slog.String("final_attempt_vendor", final.Vendor))
		}
		if final.Model != "" {
			attrs = append(attrs, slog.String("final_attempt_model", final.Model))
		}
		if final.StatusCode != 0 {
			attrs = append(attrs, slog.Int("final_attempt_status", final.StatusCode))
		}
		if final.Kind != "" {
			attrs = append(attrs, slog.String("final_attempt_error_kind", string(final.Kind)))
		}
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

func commonAttrs(eventType string, common EventCommon) []slog.Attr {
	attrs := []slog.Attr{
		slog.Int("schema_version", SchemaVersion),
		slog.String("event_type", eventType),
		slog.Time("timestamp", common.Timestamp),
		slog.String("request_id", common.RequestID),
		slog.String("operation", common.Operation),
		slog.Int("status", common.StatusCode),
		slog.Int64("duration_ms", common.DurationMS),
	}
	if common.ServiceName != "" {
		attrs = append(attrs, slog.String("service_name", common.ServiceName))
	}
	if common.ServiceVersion != "" {
		attrs = append(attrs, slog.String("service_version", common.ServiceVersion))
	}
	if common.Environment != "" {
		attrs = append(attrs, slog.String("environment", common.Environment))
	}
	return attrs
}
