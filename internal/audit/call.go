package audit

import (
	"context"
	"log/slog"

	"llmgate/internal/llmtypes"
)

// CallRecord captures the outcome of one LLM invocation. It is designed
// for delivery to a message broker (Kafka, NATS, etc.) so downstream
// systems — billing, analytics, monitoring — can consume LLM call events
// independently of the operational audit stream.
//
// Emitted only when an LLM invocation was actually attempted (authenticated
// and parsed successfully). Auth failures, bad requests, and panics before
// any vendor call do NOT produce a CallRecord — those stay in the
// operational Record only.
type CallRecord struct {
	EventCommon

	ModelRequested string
	ModelUsed      string
	Vendor         string

	RequestBytes  int64
	ResponseBytes int64

	Usage      *llmtypes.Usage
	VendorCost string

	// Attempts records the fallback chain history. Only populated when
	// len(Attempts) > 1 (single-attempt requests are noise — the
	// top-level fields already say everything).
	Attempts []llmtypes.Attempt
}

// CallRecorder receives one CallRecord per attempted LLM invocation.
// Separated from Recorder (operational audit) so message-broker
// implementations can be swapped independently.
type CallRecorder interface {
	RecordCall(ctx context.Context, r *CallRecord)
	Close() error
}

// SlogCallRecorder emits each CallRecord as one structured slog line
// with log=call so downstream consumers (Loki / ELK) can filter.
// Intended as a transitional implementation — replace with a
// Kafka/NATS recorder when the message broker is ready.
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
