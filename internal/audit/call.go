package audit

import (
	"context"
	"log/slog"

	"llmgate/internal/llmtypes"
)

// CallRecord captures the result of one gateway LLM request. Attempts contains
// the vendor/model history for that request; a CallRecord is emitted only after
// at least one vendor attempt exists.
type CallRecord struct {
	EventCommon

	ModelRequested string
	ModelUsed      string
	Vendor         string

	RequestBytes  int64
	ResponseBytes int64

	Usage      *llmtypes.Usage
	VendorCost string

	Attempts []llmtypes.Attempt
}

// CallRecorder receives one LLM call-result record per attempted LLM request.
type CallRecorder interface {
	RecordCall(ctx context.Context, r *CallRecord)
	Close() error
}

// SlogCallRecorder emits each CallRecord as one structured slog line.
type SlogCallRecorder struct {
	log *slog.Logger
}

func NewSlogCallRecorder(log *slog.Logger) *SlogCallRecorder {
	if log == nil {
		log = slog.Default()
	}
	return &SlogCallRecorder{log: log}
}

func (r *SlogCallRecorder) RecordCall(ctx context.Context, rec *CallRecord) {
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
