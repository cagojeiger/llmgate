package audit

import (
	"context"
	"log/slog"
)

// LogRecorder emits each Record as a single structured slog line at
// INFO with msg="audit". Suitable for any environment that captures
// stdout JSON; Postgres / ClickHouse / Prometheus implementations slot
// in alongside via Composite.
type LogRecorder struct {
	log *slog.Logger
}

func NewLogRecorder(log *slog.Logger) *LogRecorder {
	if log == nil {
		log = slog.Default()
	}
	return &LogRecorder{log: log}
}

func (r *LogRecorder) Record(ctx context.Context, rec *Record) {
	if rec == nil {
		return
	}

	attrs := []slog.Attr{
		slog.Time("timestamp", rec.Timestamp),
		slog.String("request_id", rec.RequestID),
		slog.String("method", rec.Method),
		slog.String("model_requested", rec.ModelRequested),
		slog.Int("status", rec.StatusCode),
		slog.Int64("duration_ms", rec.DurationMS),
		slog.Int64("request_bytes", rec.RequestBytes),
		slog.Int64("response_bytes", rec.ResponseBytes),
	}
	if rec.Vendor != "" {
		attrs = append(attrs, slog.String("vendor", rec.Vendor))
	}
	if rec.ModelUsed != "" && rec.ModelUsed != rec.ModelRequested {
		attrs = append(attrs, slog.String("model_used", rec.ModelUsed))
	}
	if rec.ErrorKind != "" {
		attrs = append(attrs, slog.String("error_kind", string(rec.ErrorKind)))
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

func (r *LogRecorder) Close() error { return nil }
