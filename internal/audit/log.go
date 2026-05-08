package audit

import (
	"context"
	"log/slog"
)

// SlogRecorder emits each Record as one structured slog line. The
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

func (r *SlogRecorder) Record(ctx context.Context, rec *Record) {
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
	if rec.AuthError != "" {
		attrs = append(attrs, slog.String("auth_error", string(rec.AuthError)))
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
		// Only surface attempts on actual fallback. Single-attempt requests
		// are noise — the top-level fields already say everything.
		attrs = append(attrs, slog.Any("attempts", rec.Attempts))
	}

	r.log.LogAttrs(ctx, slog.LevelInfo, "audit", attrs...)
}

func (r *SlogRecorder) Close() error { return nil }
