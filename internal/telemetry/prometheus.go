package telemetry

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"llmgate/internal/llmtypes"
)

var requestDurationBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5,
	1, 2.5, 5, 10, 30, 60, 120, 300,
}

var tokenUsageBuckets = []float64{
	1, 2, 4, 8, 16, 32, 64, 128, 256, 512,
	1024, 2048, 4096, 8192, 16384, 32768, 65536,
}

var tokensPerSecondBuckets = []float64{
	0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 50, 100, 200, 500,
}

// PrometheusRecorder updates RED/USE metrics on request and stream boundaries.
// Methods must stay CPU-only and non-blocking because they run inline on the
// request path.
type PrometheusRecorder struct {
	requestsTotal            *prometheus.CounterVec
	requestDuration          *prometheus.HistogramVec
	llmRequestsTotal         *prometheus.CounterVec
	llmAttemptsTotal         *prometheus.CounterVec
	llmAttemptDuration       *prometheus.HistogramVec
	llmAttemptsPerRequest    *prometheus.HistogramVec
	llmFallbackRequestsTotal *prometheus.CounterVec
	llmTokensTotal           *prometheus.CounterVec
	llmTokenUsage            *prometheus.HistogramVec
	llmIOBytesTotal          *prometheus.CounterVec
	llmGenerationDuration    *prometheus.HistogramVec
	llmOutputTokensPerSecond *prometheus.HistogramVec
	llmStreamFirstByte       *prometheus.HistogramVec
	llmStreamChunksTotal     *prometheus.CounterVec
	inflightRequests         prometheus.Gauge
	inflightStreams          prometheus.Gauge
	telemetryEnqueuedTotal   *prometheus.CounterVec
	telemetryDroppedTotal    *prometheus.CounterVec
	telemetryQueueDepth      *prometheus.GaugeVec
	telemetrySendErrorsTotal *prometheus.CounterVec
	telemetryFlushDuration   *prometheus.HistogramVec
}

func NewPrometheusRecorder(reg prometheus.Registerer) (*PrometheusRecorder, error) {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	r := &PrometheusRecorder{
		requestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "llmgate_requests_total",
				Help: "Total gateway requests by operation, status, and error kind.",
			},
			[]string{"operation", "status", "error_kind"},
		),
		requestDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "llmgate_request_duration_seconds",
				Help:    "Gateway request duration in seconds by operation, status, and error kind.",
				Buckets: requestDurationBuckets,
			},
			[]string{"operation", "status", "error_kind"},
		),
		llmRequestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "llmgate_llm_requests_total",
				Help: "Total LLM gateway requests that reached an upstream attempt by final vendor, model, status, and error kind.",
			},
			[]string{"operation", "vendor", "model", "status", "error_kind"},
		),
		llmAttemptsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "llmgate_llm_attempts_total",
				Help: "Total upstream LLM attempts by operation, vendor, model, status, and error kind.",
			},
			[]string{"operation", "vendor", "model", "status", "error_kind"},
		),
		llmAttemptDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "llmgate_llm_attempt_duration_seconds",
				Help:    "Upstream LLM attempt duration in seconds by operation, vendor, model, status, and error kind.",
				Buckets: requestDurationBuckets,
			},
			[]string{"operation", "vendor", "model", "status", "error_kind"},
		),
		llmAttemptsPerRequest: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "llmgate_llm_attempts_per_request",
				Help:    "Number of upstream LLM attempts used to serve one gateway request.",
				Buckets: []float64{1, 2, 3, 4, 5, 10},
			},
			[]string{"operation", "status", "error_kind"},
		),
		llmFallbackRequestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "llmgate_llm_fallback_requests_total",
				Help: "Total LLM gateway requests that required more than one upstream attempt.",
			},
			[]string{"operation", "status", "error_kind"},
		),
		llmTokensTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "llmgate_llm_tokens_total",
				Help: "Total LLM tokens reported by providers by operation, vendor, model, and token direction.",
			},
			[]string{"operation", "vendor", "model", "direction"},
		),
		llmTokenUsage: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "llmgate_llm_token_usage",
				Help:    "Per-request LLM token usage reported by providers by operation, vendor, model, and token direction.",
				Buckets: tokenUsageBuckets,
			},
			[]string{"operation", "vendor", "model", "direction"},
		),
		llmIOBytesTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "llmgate_llm_io_bytes_total",
				Help: "Total gateway LLM request and response bytes by operation and direction.",
			},
			[]string{"operation", "direction"},
		),
		llmGenerationDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "llmgate_llm_generation_duration_seconds",
				Help:    "Observed output generation duration in seconds. Streaming excludes time to first chunk when available; non-stream uses full attempt duration.",
				Buckets: requestDurationBuckets,
			},
			[]string{"operation", "vendor", "model", "mode"},
		),
		llmOutputTokensPerSecond: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "llmgate_llm_output_tokens_per_second",
				Help:    "Observed completion token production rate. Streaming excludes time to first chunk when available; non-stream uses full attempt duration.",
				Buckets: tokensPerSecondBuckets,
			},
			[]string{"operation", "vendor", "model", "mode"},
		),
		llmStreamFirstByte: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "llmgate_llm_stream_first_byte_seconds",
				Help:    "Time from upstream stream attempt start to first emitted chunk in seconds.",
				Buckets: requestDurationBuckets,
			},
			[]string{"operation", "vendor", "model", "status", "error_kind"},
		),
		llmStreamChunksTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "llmgate_llm_stream_chunks_total",
				Help: "Total emitted streaming chunks by operation, vendor, and model.",
			},
			[]string{"operation", "vendor", "model"},
		),
		inflightRequests: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "llmgate_inflight_requests",
				Help: "Current in-flight gateway requests.",
			},
		),
		inflightStreams: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "llmgate_inflight_streams",
				Help: "Current in-flight streaming responses.",
			},
		),
		telemetryEnqueuedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "llmgate_telemetry_events_enqueued_total",
				Help: "Total telemetry events enqueued for asynchronous delivery.",
			},
			[]string{"sink", "event_type"},
		),
		telemetryDroppedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "llmgate_telemetry_events_dropped_total",
				Help: "Total telemetry events dropped before asynchronous delivery.",
			},
			[]string{"sink", "event_type", "reason"},
		),
		telemetryQueueDepth: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "llmgate_telemetry_queue_depth",
				Help: "Current queued telemetry events awaiting asynchronous delivery.",
			},
			[]string{"sink"},
		),
		telemetrySendErrorsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "llmgate_telemetry_send_errors_total",
				Help: "Total asynchronous telemetry export errors.",
			},
			[]string{"sink", "event_type"},
		),
		telemetryFlushDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "llmgate_telemetry_flush_duration_seconds",
				Help:    "Asynchronous telemetry sink close and flush duration in seconds.",
				Buckets: requestDurationBuckets,
			},
			[]string{"sink"},
		),
	}
	metrics := []prometheus.Collector{
		r.requestsTotal,
		r.requestDuration,
		r.llmRequestsTotal,
		r.llmAttemptsTotal,
		r.llmAttemptDuration,
		r.llmAttemptsPerRequest,
		r.llmFallbackRequestsTotal,
		r.llmTokensTotal,
		r.llmTokenUsage,
		r.llmIOBytesTotal,
		r.llmGenerationDuration,
		r.llmOutputTokensPerSecond,
		r.llmStreamFirstByte,
		r.llmStreamChunksTotal,
		r.inflightRequests,
		r.inflightStreams,
		r.telemetryEnqueuedTotal,
		r.telemetryDroppedTotal,
		r.telemetryQueueDepth,
		r.telemetrySendErrorsTotal,
		r.telemetryFlushDuration,
	}
	for _, c := range metrics {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("register prometheus metric: %w", err)
		}
	}
	return r, nil
}

func (r *PrometheusRecorder) Emit(_ context.Context, event Event) {
	if r == nil || event == nil {
		return
	}
	switch rec := event.(type) {
	case *AuditEvent:
		r.emitAudit(rec)
	case *CallEvent:
		r.emitCall(rec)
	}
}

func (r *PrometheusRecorder) emitAudit(rec *AuditEvent) {
	if rec == nil {
		return
	}
	operation := labelValue(rec.Operation, "unknown")
	status := strconv.Itoa(rec.StatusCode)
	errorKind := labelValue(string(rec.Kind), "none")
	r.requestsTotal.WithLabelValues(operation, status, errorKind).Inc()
	r.requestDuration.WithLabelValues(operation, status, errorKind).Observe(float64(rec.DurationMS) / 1000)
}

func (r *PrometheusRecorder) emitCall(rec *CallEvent) {
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

func (r *PrometheusRecorder) emitTokens(operation, vendor, model string, usage *llmtypes.Usage) {
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

func (r *PrometheusRecorder) emitGenerationRate(operation, vendor, model, rawOperation string, firstByteMS int64, attempt llmtypes.Attempt) {
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

func (r *PrometheusRecorder) RequestStarted(context.Context) {
	if r != nil {
		r.inflightRequests.Inc()
	}
}

func (r *PrometheusRecorder) RequestFinished(context.Context) {
	if r != nil {
		r.inflightRequests.Dec()
	}
}

func (r *PrometheusRecorder) StreamStarted(context.Context, EventCommon) {
	if r != nil {
		r.inflightStreams.Inc()
	}
}

func (r *PrometheusRecorder) StreamFinished(context.Context, *AuditEvent, *CallEvent) {
	if r != nil {
		r.inflightStreams.Dec()
	}
}

func (r *PrometheusRecorder) Close() error { return nil }

func (r *PrometheusRecorder) AsyncEventEnqueued(sinkName, eventType string) {
	if r != nil {
		r.telemetryEnqueuedTotal.WithLabelValues(labelValue(sinkName, "unknown"), labelValue(eventType, "unknown")).Inc()
	}
}

func (r *PrometheusRecorder) AsyncEventDropped(sinkName, eventType, reason string) {
	if r != nil {
		r.telemetryDroppedTotal.WithLabelValues(labelValue(sinkName, "unknown"), labelValue(eventType, "unknown"), labelValue(reason, "unknown")).Inc()
	}
}

func (r *PrometheusRecorder) AsyncQueueDepth(sinkName string, depth int) {
	if r != nil {
		r.telemetryQueueDepth.WithLabelValues(labelValue(sinkName, "unknown")).Set(float64(depth))
	}
}

func (r *PrometheusRecorder) AsyncSendError(sinkName, eventType string) {
	if r != nil {
		r.telemetrySendErrorsTotal.WithLabelValues(labelValue(sinkName, "unknown"), labelValue(eventType, "unknown")).Inc()
	}
}

func (r *PrometheusRecorder) AsyncFlushFinished(sinkName string, duration time.Duration) {
	if r != nil {
		r.telemetryFlushDuration.WithLabelValues(labelValue(sinkName, "unknown")).Observe(duration.Seconds())
	}
}

func labelValue(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
