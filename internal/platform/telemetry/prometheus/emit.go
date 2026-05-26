package promtelemetry

import (
	"context"
	"strconv"

	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/telemetry"
)

func (r *Recorder) Emit(_ context.Context, event telemetry.Event) {
	if r == nil || event == nil {
		return
	}
	switch rec := event.(type) {
	case *telemetry.AuditEvent:
		r.emitAudit(rec)
	case *telemetry.CallEvent:
		r.emitCall(rec)
	}
}

func (r *Recorder) emitAudit(rec *telemetry.AuditEvent) {
	if rec == nil {
		return
	}
	operation := labelValue(rec.Operation, "unknown")
	status := strconv.Itoa(rec.StatusCode)
	errorKind := labelValue(string(rec.Kind), "none")
	r.requestsTotal.WithLabelValues(operation, status, errorKind).Inc()
	r.requestDuration.WithLabelValues(operation, status, errorKind).Observe(float64(rec.DurationMS) / 1000)
}

func (r *Recorder) emitCall(rec *telemetry.CallEvent) {
	if rec == nil {
		return
	}
	operation := labelValue(rec.Operation, "unknown")
	finalVendor := labelValue(rec.Vendor, "unknown")
	finalModel := labelValue(rec.ModelUsed, "unknown")
	finalStatus := strconv.Itoa(rec.StatusCode)
	finalErrorKind := labelValue(string(rec.Kind), "none")
	if rec.RequestBytes > 0 {
		r.llmIOBytesTotal.WithLabelValues(operation, "request").Add(float64(rec.RequestBytes))
	}
	if rec.ResponseBytes > 0 {
		r.llmIOBytesTotal.WithLabelValues(operation, "response").Add(float64(rec.ResponseBytes))
	}
	if len(rec.Attempts) > 0 {
		r.llmRequestsTotal.WithLabelValues(operation, finalVendor, finalModel, finalStatus, finalErrorKind).Inc()
		r.llmAttemptsPerRequest.WithLabelValues(operation, finalStatus, finalErrorKind).Observe(float64(len(rec.Attempts)))
		if len(rec.Attempts) > 1 {
			r.llmFallbackRequestsTotal.WithLabelValues(operation, finalStatus, finalErrorKind).Inc()
		}
	}
	for _, attempt := range rec.Attempts {
		vendor := labelValue(attempt.Vendor, "unknown")
		model := labelValue(attempt.Model, "unknown")
		status := strconv.Itoa(attempt.StatusCode)
		errorKind := labelValue(string(attempt.Kind), "none")
		r.llmAttemptsTotal.WithLabelValues(operation, vendor, model, status, errorKind).Inc()
		r.llmAttemptDuration.WithLabelValues(operation, vendor, model, status, errorKind).Observe(float64(attempt.DurationMS) / 1000)
		if attempt.Usage != nil {
			r.emitTokens(operation, vendor, model, attempt.Usage)
			r.emitGenerationRate(operation, vendor, model, rec.Operation, rec.FirstByteMS, attempt)
		}
	}
	if rec.Usage != nil && len(rec.Attempts) == 0 {
		r.emitTokens(operation, finalVendor, finalModel, rec.Usage)
	}
	if rec.FirstByteMS > 0 {
		r.llmStreamFirstByte.WithLabelValues(operation, finalVendor, finalModel, finalStatus, finalErrorKind).Observe(float64(rec.FirstByteMS) / 1000)
	}
	if rec.StreamChunks > 0 {
		r.llmStreamChunksTotal.WithLabelValues(operation, finalVendor, finalModel).Add(float64(rec.StreamChunks))
	}
}

func (r *Recorder) emitTokens(operation, vendor, model string, usage *llmtypes.Usage) {
	if usage == nil {
		return
	}
	if usage.PromptTokens > 0 {
		r.llmTokensTotal.WithLabelValues(operation, vendor, model, "prompt").Add(float64(usage.PromptTokens))
		r.llmTokenUsage.WithLabelValues(operation, vendor, model, "prompt").Observe(float64(usage.PromptTokens))
	}
	if usage.CompletionTokens > 0 {
		r.llmTokensTotal.WithLabelValues(operation, vendor, model, "completion").Add(float64(usage.CompletionTokens))
		r.llmTokenUsage.WithLabelValues(operation, vendor, model, "completion").Observe(float64(usage.CompletionTokens))
	}
}

func (r *Recorder) emitGenerationRate(operation, vendor, model, rawOperation string, firstByteMS int64, attempt llmtypes.Attempt) {
	if attempt.Usage == nil || attempt.Usage.CompletionTokens <= 0 || attempt.DurationMS <= 0 {
		return
	}
	mode := "end_to_end"
	generationMS := attempt.DurationMS
	if rawOperation == "chat.completions.stream" && firstByteMS > 0 && attempt.DurationMS > firstByteMS {
		mode = "stream_after_first_chunk"
		generationMS = attempt.DurationMS - firstByteMS
	}
	if generationMS <= 0 {
		return
	}
	generationSeconds := float64(generationMS) / 1000
	tokensPerSecond := float64(attempt.Usage.CompletionTokens) / generationSeconds
	r.llmGenerationDuration.WithLabelValues(operation, vendor, model, mode).Observe(generationSeconds)
	r.llmOutputTokensPerSecond.WithLabelValues(operation, vendor, model, mode).Observe(tokensPerSecond)
}

func labelValue(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
