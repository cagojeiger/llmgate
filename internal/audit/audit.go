// Package audit records one row per gateway request.
package audit

import (
	"context"
	"time"

	"llmgate/internal/provider"
)

// Record captures the per-request audit payload.
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

	Attempts []provider.Attempt
}

// Recorder receives one Record per gateway request.
type Recorder interface {
	Record(ctx context.Context, r *Record)
	Close() error
}

// Nop drops every record. Default when no recorder is configured.
type Nop struct{}

func (Nop) Record(context.Context, *Record) {}
func (Nop) Close() error                    { return nil }
