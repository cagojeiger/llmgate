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
//
// ModelRequested is the name on the incoming request (may be a logical
// alias resolved by the router). Vendor + ModelUsed identify the upstream
// that actually returned the body the client received. On total failure
// (all attempts errored), Vendor / ModelUsed may be empty.
//
// Attempts captures every model that was tried in order, including failed
// ones. Downstream consumers can compute "billable tokens" by their own
// policy — the gateway emits facts only.
type Record struct {
	Timestamp time.Time
	RequestID string

	Method         string // "chat.completions" | "chat.completions.stream"
	ModelRequested string

	Vendor    string
	ModelUsed string

	StatusCode int
	ErrorKind  provider.Kind
	DurationMS int64

	RequestBytes  int64
	ResponseBytes int64

	Usage      *provider.Usage
	VendorCost string

	Attempts []Attempt
}

// Attempt records one upstream call within a single gateway request. A
// non-fallback request produces exactly one Attempt; a fallback chain
// produces N — typically N-1 with errors followed by one success.
//
// Usage may be nil when the upstream rejected before generation (4xx,
// pre-stream 5xx). For mid-stream truncation, adapters surface partial
// usage via Stream.Summary so the value here can be non-nil even with
// a non-success ErrorKind.
type Attempt struct {
	Vendor       string
	Model        string
	StartedAt    time.Time
	DurationMS   int64
	StatusCode   int
	ErrorKind    provider.Kind
	Usage        *provider.Usage
	VendorCost   string
	StreamChunks int
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
