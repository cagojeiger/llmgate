package slogtelemetry

import (
	"context"
	"log/slog"

	"llmgate/internal/domain/telemetry"
)

// Sink routes audit and call events to their log-specific sloggers while
// preserving the existing Loki-friendly line shape.
type Sink struct {
	auditLog *slog.Logger
	callLog  *slog.Logger
}

func NewSink(auditLog, callLog *slog.Logger) *Sink {
	if auditLog == nil {
		auditLog = slog.Default()
	}
	if callLog == nil {
		callLog = slog.Default()
	}
	return &Sink{auditLog: auditLog, callLog: callLog}
}

func (s *Sink) Emit(ctx context.Context, event telemetry.Event) {
	switch rec := event.(type) {
	case *telemetry.AuditEvent:
		s.recordAudit(ctx, rec)
	case *telemetry.CallEvent:
		s.recordCall(ctx, rec)
	}
}

func (s *Sink) Close() error { return nil }

func (s *Sink) recordAudit(ctx context.Context, rec *telemetry.AuditEvent) {
	if rec == nil {
		return
	}

	attrs := commonAttrs(telemetry.EventTypeAudit, rec.EventCommon)
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

	s.auditLog.LogAttrs(ctx, slog.LevelInfo, "audit", attrs...)
}

func (s *Sink) recordCall(ctx context.Context, rec *telemetry.CallEvent) {
	if rec == nil {
		return
	}

	attrs := append(commonAttrs(telemetry.EventTypeCall, rec.EventCommon),
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
	attrs = append(attrs, slog.Int("attempts_count", telemetry.AttemptsCount(rec)))
	if final, ok := telemetry.FinalAttempt(rec); ok {
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

	s.callLog.LogAttrs(ctx, slog.LevelInfo, "call", attrs...)
}

func commonAttrs(eventType string, common telemetry.EventCommon) []slog.Attr {
	attrs := []slog.Attr{
		slog.Int("schema_version", telemetry.SchemaVersion),
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
