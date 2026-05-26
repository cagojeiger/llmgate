package promtelemetry

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
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

// Recorder updates RED/USE metrics on request and stream boundaries.
// Methods must stay CPU-only and non-blocking because they run inline on the
// request path.
type Recorder struct {
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
}

func NewRecorder(reg prometheus.Registerer) (*Recorder, error) {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	r := &Recorder{
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
	}
	for _, c := range metrics {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("register prometheus metric: %w", err)
		}
	}
	return r, nil
}
