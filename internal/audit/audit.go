// Package audit records one row per gateway request.
package audit

import (
	"context"
	"time"

	"llmgate/internal/llmtypes"
)

// Record captures the per-request audit payload.
type Record struct {
	Timestamp time.Time
	RequestID string

	Method         string // "chat.completions" | "chat.completions.stream"
	ModelRequested string

	// ConsumerName identifies the registered caller (matched yaml `name` in
	// consumers/) for this request. Empty when the request was rejected at
	// the auth boundary; the record is still emitted in that case so
	// brute-force / mis-configured-key activity is observable. ConsumerKeyID
	// is the first 8 hex chars of the matched hash (sha256), useful for
	// detecting which key in a rotating set was actually used. AuthError
	// names the failure mode at the auth boundary ("missing" / "format" /
	// "unknown") and is empty on success — it stays separate from ErrorKind
	// because the wire response collapses every auth failure to 401, so
	// the kind only lives in the audit / access-log surface.
	ConsumerName  string
	ConsumerKeyID string
	AuthError     string

	Vendor    string
	ModelUsed string

	StatusCode int
	ErrorKind  llmtypes.ErrorKind
	DurationMS int64

	RequestBytes  int64
	ResponseBytes int64

	Usage      *llmtypes.Usage
	VendorCost string

	Attempts []llmtypes.Attempt
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
