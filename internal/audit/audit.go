// Package audit records one row per gateway request: who, what model,
// outcome, latency, bytes, tokens, cost. Implementations vary
// (slog, Postgres, ClickHouse, Prometheus); the interface stays one.
package audit

import (
	"context"
	"time"

	"llmgate/internal/provider"
)

// Record captures the per-request audit payload. Populated by the handler
// across the request lifecycle and emitted exactly once via Recorder.
type Record struct {
	Timestamp time.Time
	RequestID string

	Method string // "chat.completions" | "chat.completions.stream"
	Model  string

	StatusCode int
	ErrorKind  provider.Kind
	DurationMS int64

	RequestBytes  int64
	ResponseBytes int64

	Usage      *provider.Usage
	VendorCost string
}

// Recorder receives one Record per gateway request. Implementations must
// not block the caller — they should buffer or fail silently and log
// internally. Returning an error from Record was deliberately rejected.
type Recorder interface {
	Record(ctx context.Context, r *Record)
	Close() error
}

// Nop drops every record. Default when no recorder is configured.
type Nop struct{}

func (Nop) Record(context.Context, *Record) {}
func (Nop) Close() error                    { return nil }
