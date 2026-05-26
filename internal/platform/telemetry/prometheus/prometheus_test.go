package promtelemetry

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	llmresult "llmgate/internal/domain/llmresult/schema"
	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/telemetry"
)

func TestPrometheusRecorder_RecordAudit(t *testing.T) {
	reg := prometheus.NewRegistry()
	r, err := NewRecorder(reg)
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}

	r.Emit(context.Background(), &telemetry.AuditEvent{
		EventCommon: telemetry.EventCommon{
			Operation:  "chat.completions",
			StatusCode: http.StatusBadGateway,
			Kind:       llmtypes.KindUpstream,
			DurationMS: 1500,
		},
	})

	labels := map[string]string{
		"operation":  "chat.completions",
		"status":     "502",
		"error_kind": "upstream",
	}
	if got := findCounterValue(t, reg, "llmgate_requests_total", labels); got != 1 {
		t.Fatalf("requests counter = %v, want 1", got)
	}
	count, sum := findHistogramCountAndSum(t, reg, "llmgate_request_duration_seconds", labels)
	if count != 1 {
		t.Fatalf("duration count = %d, want 1", count)
	}
	if sum != 1.5 {
		t.Fatalf("duration sum = %v, want 1.5", sum)
	}
}

func TestPrometheusRecorder_LabelsEmptyErrorKindAsNone(t *testing.T) {
	reg := prometheus.NewRegistry()
	r, err := NewRecorder(reg)
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}

	r.Emit(context.Background(), &telemetry.AuditEvent{
		EventCommon: telemetry.EventCommon{
			Operation:  "chat.completions.stream",
			StatusCode: http.StatusOK,
			DurationMS: 250,
		},
	})

	if got := findCounterValue(t, reg, "llmgate_requests_total", map[string]string{
		"operation":  "chat.completions.stream",
		"status":     "200",
		"error_kind": "none",
	}); got != 1 {
		t.Fatalf("requests counter = %v, want 1", got)
	}
}

func TestPrometheusRecorder_LifecycleGauges(t *testing.T) {
	reg := prometheus.NewRegistry()
	r, err := NewRecorder(reg)
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}

	r.RequestStarted(context.Background())
	r.RequestStarted(context.Background())
	r.RequestFinished(context.Background())
	r.StreamStarted(context.Background(), telemetry.EventCommon{})
	r.StreamFinished(context.Background(), nil, nil)

	if got := findGaugeValue(t, reg, "llmgate_inflight_requests"); got != 1 {
		t.Fatalf("inflight requests = %v, want 1", got)
	}
	if got := findGaugeValue(t, reg, "llmgate_inflight_streams"); got != 0 {
		t.Fatalf("inflight streams = %v, want 0", got)
	}
}

func TestPrometheusRecorder_RecordCallAttemptsAndTokens(t *testing.T) {
	reg := prometheus.NewRegistry()
	r, err := NewRecorder(reg)
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}

	started := time.Now().Add(-2 * time.Second)
	r.Emit(context.Background(), &telemetry.CallEvent{
		EventCommon: telemetry.EventCommon{
			Operation:  "chat.completions",
			StatusCode: http.StatusOK,
			DurationMS: 2000,
		},
		Vendor:        "opencode",
		ModelUsed:     "deepseek-v4-flash",
		RequestBytes:  128,
		ResponseBytes: 512,
		Attempts: []llmtypes.Attempt{{
			Vendor:     "opencode",
			Model:      "deepseek-v4-flash",
			StartedAt:  started,
			DurationMS: 2000,
			StatusCode: http.StatusOK,
			Usage:      &llmtypes.Usage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18},
		}},
	})

	labels := map[string]string{
		"operation":  "chat.completions",
		"vendor":     "opencode",
		"model":      "deepseek-v4-flash",
		"status":     "200",
		"error_kind": "none",
	}
	if got := findCounterValue(t, reg, "llmgate_llm_attempts_total", labels); got != 1 {
		t.Fatalf("attempts counter = %v, want 1", got)
	}
	if got := findCounterValue(t, reg, "llmgate_llm_requests_total", labels); got != 1 {
		t.Fatalf("llm requests counter = %v, want 1", got)
	}
	count, sum := findHistogramCountAndSum(t, reg, "llmgate_llm_attempt_duration_seconds", labels)
	if count != 1 {
		t.Fatalf("attempt duration count = %d, want 1", count)
	}
	if sum != 2 {
		t.Fatalf("attempt duration sum = %v, want 2", sum)
	}
	if got := findCounterValue(t, reg, "llmgate_llm_tokens_total", map[string]string{
		"operation": "chat.completions",
		"vendor":    "opencode",
		"model":     "deepseek-v4-flash",
		"direction": "prompt",
	}); got != 11 {
		t.Fatalf("prompt tokens = %v, want 11", got)
	}
	count, sum = findHistogramCountAndSum(t, reg, "llmgate_llm_token_usage", map[string]string{
		"operation": "chat.completions",
		"vendor":    "opencode",
		"model":     "deepseek-v4-flash",
		"direction": "prompt",
	})
	if count != 1 {
		t.Fatalf("prompt token usage count = %d, want 1", count)
	}
	if sum != 11 {
		t.Fatalf("prompt token usage sum = %v, want 11", sum)
	}
	if got := findCounterValue(t, reg, "llmgate_llm_tokens_total", map[string]string{
		"operation": "chat.completions",
		"vendor":    "opencode",
		"model":     "deepseek-v4-flash",
		"direction": "completion",
	}); got != 7 {
		t.Fatalf("completion tokens = %v, want 7", got)
	}
	if got := findCounterValue(t, reg, "llmgate_llm_io_bytes_total", map[string]string{
		"operation": "chat.completions",
		"direction": "request",
	}); got != 128 {
		t.Fatalf("request bytes = %v, want 128", got)
	}
	if got := findCounterValue(t, reg, "llmgate_llm_io_bytes_total", map[string]string{
		"operation": "chat.completions",
		"direction": "response",
	}); got != 512 {
		t.Fatalf("response bytes = %v, want 512", got)
	}
	count, sum = findHistogramCountAndSum(t, reg, "llmgate_llm_generation_duration_seconds", map[string]string{
		"operation": "chat.completions",
		"vendor":    "opencode",
		"model":     "deepseek-v4-flash",
		"mode":      "end_to_end",
	})
	if count != 1 {
		t.Fatalf("generation duration count = %d, want 1", count)
	}
	if sum != 2 {
		t.Fatalf("generation duration sum = %v, want 2", sum)
	}
	count, sum = findHistogramCountAndSum(t, reg, "llmgate_llm_output_tokens_per_second", map[string]string{
		"operation": "chat.completions",
		"vendor":    "opencode",
		"model":     "deepseek-v4-flash",
		"mode":      "end_to_end",
	})
	if count != 1 {
		t.Fatalf("output tps count = %d, want 1", count)
	}
	if sum != 3.5 {
		t.Fatalf("output tps sum = %v, want 3.5", sum)
	}
}

func TestPrometheusRecorder_RecordFallbackRequest(t *testing.T) {
	reg := prometheus.NewRegistry()
	r, err := NewRecorder(reg)
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}

	r.Emit(context.Background(), &telemetry.CallEvent{
		EventCommon: telemetry.EventCommon{
			Operation:  "chat.completions",
			StatusCode: http.StatusOK,
		},
		Vendor:    "opencode",
		ModelUsed: "deepseek-v4-flash",
		Attempts: []llmtypes.Attempt{
			{Vendor: "opencode", Model: "deepseek-v4-pro", StatusCode: http.StatusTooManyRequests, Kind: llmtypes.KindRateLimit},
			{Vendor: "opencode", Model: "deepseek-v4-flash", StatusCode: http.StatusOK},
		},
	})

	labels := map[string]string{
		"operation":  "chat.completions",
		"status":     "200",
		"error_kind": "none",
	}
	if got := findCounterValue(t, reg, "llmgate_llm_fallback_requests_total", labels); got != 1 {
		t.Fatalf("fallback requests = %v, want 1", got)
	}
	count, sum := findHistogramCountAndSum(t, reg, "llmgate_llm_attempts_per_request", labels)
	if count != 1 {
		t.Fatalf("attempts per request count = %d, want 1", count)
	}
	if sum != 2 {
		t.Fatalf("attempts per request sum = %v, want 2", sum)
	}
	if got := findCounterValue(t, reg, "llmgate_llm_requests_total", map[string]string{
		"operation":  "chat.completions",
		"vendor":     "opencode",
		"model":      "deepseek-v4-flash",
		"status":     "200",
		"error_kind": "none",
	}); got != 1 {
		t.Fatalf("final llm requests = %v, want 1", got)
	}
}

func TestPrometheusRecorder_RecordStreamFirstByteAndChunks(t *testing.T) {
	reg := prometheus.NewRegistry()
	r, err := NewRecorder(reg)
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}

	r.Emit(context.Background(), &telemetry.CallEvent{
		EventCommon: telemetry.EventCommon{
			Operation:  "chat.completions.stream",
			StatusCode: http.StatusOK,
		},
		Vendor:       "anthropic",
		ModelUsed:    "minimax-m2.5",
		FirstByteMS:  250,
		StreamChunks: 3,
		Attempts: []llmtypes.Attempt{{
			Vendor:     "anthropic",
			Model:      "minimax-m2.5",
			DurationMS: 1250,
			StatusCode: http.StatusOK,
			Usage:      &llmtypes.Usage{CompletionTokens: 4},
		}},
	})

	labels := map[string]string{
		"operation":  "chat.completions.stream",
		"vendor":     "anthropic",
		"model":      "minimax-m2.5",
		"status":     "200",
		"error_kind": "none",
	}
	count, sum := findHistogramCountAndSum(t, reg, "llmgate_llm_stream_first_byte_seconds", labels)
	if count != 1 {
		t.Fatalf("first byte count = %d, want 1", count)
	}
	if sum != 0.25 {
		t.Fatalf("first byte sum = %v, want 0.25", sum)
	}
	if got := findCounterValue(t, reg, "llmgate_llm_stream_chunks_total", map[string]string{
		"operation": "chat.completions.stream",
		"vendor":    "anthropic",
		"model":     "minimax-m2.5",
	}); got != 3 {
		t.Fatalf("stream chunks = %v, want 3", got)
	}
	count, sum = findHistogramCountAndSum(t, reg, "llmgate_llm_output_tokens_per_second", map[string]string{
		"operation": "chat.completions.stream",
		"vendor":    "anthropic",
		"model":     "minimax-m2.5",
		"mode":      "stream_after_first_chunk",
	})
	if count != 1 {
		t.Fatalf("stream output tps count = %d, want 1", count)
	}
	if sum != 4 {
		t.Fatalf("stream output tps sum = %v, want 4", sum)
	}
}

func TestPrometheusRecorder_RecordNil(t *testing.T) {
	reg := prometheus.NewRegistry()
	r, err := NewRecorder(reg)
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}

	r.Emit(context.Background(), nil)

	metricFamilies, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range metricFamilies {
		if mf.GetName() != "llmgate_requests_total" && mf.GetName() != "llmgate_request_duration_seconds" {
			continue
		}
		if len(mf.GetMetric()) != 0 {
			t.Fatalf("%s has %d metrics after nil audit record, want 0", mf.GetName(), len(mf.GetMetric()))
		}
	}
}

func TestPrometheusRecorder_RecordLLMResultDrop(t *testing.T) {
	reg := prometheus.NewRegistry()
	r, err := NewRecorder(reg)
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}

	r.ObserveLLMResultDropped(&llmresult.Event{PayloadMode: "metadata_only"}, "queue_full")

	got := findCounterValue(t, reg, "llmgate_llmresult_events_dropped_total", map[string]string{
		"reason":       "queue_full",
		"payload_mode": "metadata_only",
	})
	if got != 1 {
		t.Fatalf("drop counter = %v, want 1", got)
	}
}

func TestPrometheusRecorder_RecordLLMResultPublishFailure(t *testing.T) {
	reg := prometheus.NewRegistry()
	r, err := NewRecorder(reg)
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}

	r.ObserveLLMResultPublishFailed(&llmresult.Event{PayloadMode: "full"}, "publish_failed")

	got := findCounterValue(t, reg, "llmgate_llmresult_events_publish_failed_total", map[string]string{
		"reason":       "publish_failed",
		"payload_mode": "full",
	})
	if got != 1 {
		t.Fatalf("publish failure counter = %v, want 1", got)
	}
}

func BenchmarkPrometheusRecorder_EmitCall(b *testing.B) {
	reg := prometheus.NewRegistry()
	r, err := NewRecorder(reg)
	if err != nil {
		b.Fatalf("NewRecorder: %v", err)
	}
	ctx := context.Background()
	ev := &telemetry.CallEvent{
		EventCommon: telemetry.EventCommon{
			Operation:  "chat.completions.stream",
			StatusCode: http.StatusOK,
			DurationMS: 2200,
		},
		Vendor:        "opencode",
		ModelUsed:     "deepseek-v4-flash",
		RequestBytes:  256,
		ResponseBytes: 1024,
		FirstByteMS:   300,
		StreamChunks:  8,
		Attempts: []llmtypes.Attempt{{
			Vendor:     "opencode",
			Model:      "deepseek-v4-flash",
			DurationMS: 2200,
			StatusCode: http.StatusOK,
			Usage:      &llmtypes.Usage{PromptTokens: 120, CompletionTokens: 32, TotalTokens: 152},
		}},
	}

	r.Emit(ctx, ev)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Emit(ctx, ev)
	}
}

func findCounterValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	metricFamilies, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range metricFamilies {
		if mf.GetName() != name {
			continue
		}
		for _, metric := range mf.GetMetric() {
			gotLabels := make(map[string]string, len(metric.GetLabel()))
			for _, pair := range metric.GetLabel() {
				gotLabels[pair.GetName()] = pair.GetValue()
			}
			if labelsMatch(gotLabels, labels) {
				return metric.GetCounter().GetValue()
			}
		}
		t.Fatalf("metric %s exists but no sample matched labels %+v", name, labels)
	}
	t.Fatalf("metric %s not found", name)
	return 0
}

func findGaugeValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	metricFamilies, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range metricFamilies {
		if mf.GetName() != name {
			continue
		}
		metrics := mf.GetMetric()
		if len(metrics) != 1 {
			t.Fatalf("metric %s samples = %d, want 1", name, len(metrics))
		}
		return metrics[0].GetGauge().GetValue()
	}
	t.Fatalf("metric %s not found", name)
	return 0
}

func findHistogramCountAndSum(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) (uint64, float64) {
	t.Helper()
	metricFamilies, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range metricFamilies {
		if mf.GetName() != name {
			continue
		}
		for _, metric := range mf.GetMetric() {
			gotLabels := make(map[string]string, len(metric.GetLabel()))
			for _, pair := range metric.GetLabel() {
				gotLabels[pair.GetName()] = pair.GetValue()
			}
			if labelsMatch(gotLabels, labels) {
				hist := metric.GetHistogram()
				return hist.GetSampleCount(), hist.GetSampleSum()
			}
		}
		t.Fatalf("metric %s exists but no sample matched labels %+v", name, labels)
	}
	t.Fatalf("metric %s not found", name)
	return 0, 0
}

func labelsMatch(got map[string]string, want map[string]string) bool {
	if len(want) == 0 {
		return true
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}
