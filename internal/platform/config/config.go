package config

import (
	"log/slog"
	"time"
)

type Server struct {
	Addr string
	// Environment labels logs and future telemetry events with the
	// deployment boundary operators search by (local, staging, prod).
	Environment string
	// ShutdownDrainTimeout caps how long graceful shutdown waits for
	// in-flight requests to finish before force-closing any survivors.
	// Default 5m comfortably covers typical LLM streams; the
	// orchestrator's terminationGracePeriodSeconds (k8s) /
	// stop_grace_period (compose) should be set slightly larger so the
	// app-side force close fires before SIGKILL.
	ShutdownDrainTimeout time.Duration
	LogLevel             slog.Level

	// Routing fallback, breaker, and timeout settings.
	FallbackOn        []string
	CircuitFailures   int
	CircuitOpen       time.Duration
	CircuitMaxOpen    time.Duration
	CircuitJitter     float64
	RequestTimeout    time.Duration
	CompleteTimeout   time.Duration
	StreamIdleTimeout time.Duration
	// MaxRequestBytes caps the chat request body. Default 10 MiB — sized for
	// base64 image content (a phone photo is 2-5 MB, +33% base64), which the
	// old 1 MiB cap rejected at the gate.
	MaxRequestBytes int64
	MetricsEnabled  bool

	// Finalized LLM result event publishing.
	LLMResultAsyncQueueSize int
	LLMResultAsyncBatchSize int
	LLMResultAsyncFlush     time.Duration
	// LLMResultAsyncEmitTimeout caps one downstream Emit from the async
	// worker so a stuck sink cannot freeze the drain loop.
	LLMResultAsyncEmitTimeout time.Duration
	// LLMResultAsyncCloseTimeout caps Close()'s wait on the worker
	// goroutine. Operators sizing terminationGracePeriodSeconds should
	// budget ShutdownDrainTimeout + this value + a small margin.
	LLMResultAsyncCloseTimeout time.Duration

	// Audit sink: local-rotate + best-effort upload of finalized result
	// events (internal/platform/audit). Empty AuditDir disables it. Empty
	// AuditS3Endpoint runs it local-only (rolling log, no upload). Result
	// events carry full prompt/completion bodies.
	AuditDir               string
	AuditRotateInterval    time.Duration
	AuditRotateMaxBytes    int64
	AuditUploadInterval    time.Duration
	AuditRetention         time.Duration
	AuditDiskCap           int64
	AuditCompression       string
	AuditUploadConcurrency int
	AuditS3Endpoint        string
	AuditS3Bucket          string
	AuditS3Region          string
	AuditS3AccessKey       string
	AuditS3SecretKey       string
	AuditS3Prefix          string
	AuditS3UseSSL          bool
	AuditS3PathStyle       bool
}

func LoadServer() (*Server, error) {
	drainTimeout, err := positiveDuration("LLMGATE_SHUTDOWN_DRAIN_TIMEOUT", "5m")
	if err != nil {
		return nil, err
	}
	logLevel, err := parseLogLevel("LLMGATE_LOG_LEVEL", "info")
	if err != nil {
		return nil, err
	}
	circuitFailures, err := nonNegativeInt("LLMGATE_CIRCUIT_FAILURES", "3")
	if err != nil {
		return nil, err
	}
	circuitOpen, err := nonNegativeDuration("LLMGATE_CIRCUIT_OPEN_DURATION", "30s")
	if err != nil {
		return nil, err
	}
	circuitMaxOpen, err := nonNegativeDuration("LLMGATE_CIRCUIT_MAX_OPEN_DURATION", "5m")
	if err != nil {
		return nil, err
	}
	circuitJitter, err := ratio("LLMGATE_CIRCUIT_JITTER", "0.2")
	if err != nil {
		return nil, err
	}
	requestTimeout, err := positiveDuration("LLMGATE_REQUEST_TIMEOUT", "5m")
	if err != nil {
		return nil, err
	}
	completeTimeout, err := positiveDuration("LLMGATE_COMPLETE_TIMEOUT", "1m")
	if err != nil {
		return nil, err
	}
	streamIdleTimeout, err := positiveDuration("LLMGATE_STREAM_IDLE_TIMEOUT", "1m")
	if err != nil {
		return nil, err
	}
	metricsEnabled, err := boolValue("LLMGATE_METRICS_ENABLED", "false")
	if err != nil {
		return nil, err
	}
	maxRequestBytes, err := positiveInt64("LLMGATE_MAX_REQUEST_BYTES", "10485760")
	if err != nil {
		return nil, err
	}
	llmResultQueueSize, err := nonNegativeInt("LLMGATE_LLMRESULT_ASYNC_QUEUE_SIZE", "1000")
	if err != nil {
		return nil, err
	}
	llmResultBatchSize, err := nonNegativeInt("LLMGATE_LLMRESULT_ASYNC_BATCH_SIZE", "100")
	if err != nil {
		return nil, err
	}
	llmResultFlush, err := nonNegativeDuration("LLMGATE_LLMRESULT_ASYNC_FLUSH_INTERVAL", "1s")
	if err != nil {
		return nil, err
	}
	llmResultEmitTimeout, err := positiveDuration("LLMGATE_LLMRESULT_ASYNC_EMIT_TIMEOUT", "10s")
	if err != nil {
		return nil, err
	}
	llmResultCloseTimeout, err := positiveDuration("LLMGATE_LLMRESULT_ASYNC_CLOSE_TIMEOUT", "60s")
	if err != nil {
		return nil, err
	}
	auditRotateInterval, err := positiveDuration("LLMGATE_AUDIT_ROTATE_INTERVAL", "10m")
	if err != nil {
		return nil, err
	}
	auditRotateMaxBytes, err := positiveInt64("LLMGATE_AUDIT_ROTATE_MAX_BYTES", "134217728")
	if err != nil {
		return nil, err
	}
	auditUploadInterval, err := positiveDuration("LLMGATE_AUDIT_UPLOAD_INTERVAL", "30s")
	if err != nil {
		return nil, err
	}
	auditRetention, err := positiveDuration("LLMGATE_AUDIT_RETENTION", "168h")
	if err != nil {
		return nil, err
	}
	auditDiskCap, err := positiveInt64("LLMGATE_AUDIT_DISK_CAP", "5368709120")
	if err != nil {
		return nil, err
	}
	auditUploadConcurrency, err := nonNegativeInt("LLMGATE_AUDIT_UPLOAD_CONCURRENCY", "4")
	if err != nil {
		return nil, err
	}
	auditS3UseSSL, err := boolValue("LLMGATE_AUDIT_S3_USE_SSL", "false")
	if err != nil {
		return nil, err
	}
	auditS3PathStyle, err := boolValue("LLMGATE_AUDIT_S3_PATH_STYLE", "true")
	if err != nil {
		return nil, err
	}

	cfg := &Server{
		Addr:                       orDefault("LLMGATE_ADDR", ":8080"),
		Environment:                orDefault("LLMGATE_ENVIRONMENT", "local"),
		ShutdownDrainTimeout:       drainTimeout,
		LogLevel:                   logLevel,
		FallbackOn:                 parseCSV("LLMGATE_FALLBACK_ON", "rate_limit,upstream,timeout,network"),
		CircuitFailures:            circuitFailures,
		CircuitOpen:                circuitOpen,
		CircuitMaxOpen:             circuitMaxOpen,
		CircuitJitter:              circuitJitter,
		RequestTimeout:             requestTimeout,
		CompleteTimeout:            completeTimeout,
		StreamIdleTimeout:          streamIdleTimeout,
		MaxRequestBytes:            maxRequestBytes,
		MetricsEnabled:             metricsEnabled,
		LLMResultAsyncQueueSize:    llmResultQueueSize,
		LLMResultAsyncBatchSize:    llmResultBatchSize,
		LLMResultAsyncFlush:        llmResultFlush,
		LLMResultAsyncEmitTimeout:  llmResultEmitTimeout,
		LLMResultAsyncCloseTimeout: llmResultCloseTimeout,
		AuditDir:                   orDefault("LLMGATE_AUDIT_DIR", ""),
		AuditRotateInterval:        auditRotateInterval,
		AuditRotateMaxBytes:        auditRotateMaxBytes,
		AuditUploadInterval:        auditUploadInterval,
		AuditRetention:             auditRetention,
		AuditDiskCap:               auditDiskCap,
		AuditCompression:           orDefault("LLMGATE_AUDIT_COMPRESSION", "gzip"),
		AuditUploadConcurrency:     auditUploadConcurrency,
		AuditS3Endpoint:            orDefault("LLMGATE_AUDIT_S3_ENDPOINT", ""),
		AuditS3Bucket:              orDefault("LLMGATE_AUDIT_S3_BUCKET", ""),
		AuditS3Region:              orDefault("LLMGATE_AUDIT_S3_REGION", "us-east-1"),
		AuditS3AccessKey:           orDefault("LLMGATE_AUDIT_S3_ACCESS_KEY", ""),
		AuditS3SecretKey:           orDefault("LLMGATE_AUDIT_S3_SECRET_KEY", ""),
		AuditS3Prefix:              orDefault("LLMGATE_AUDIT_S3_PREFIX", ""),
		AuditS3UseSSL:              auditS3UseSSL,
		AuditS3PathStyle:           auditS3PathStyle,
	}
	return cfg, nil
}
