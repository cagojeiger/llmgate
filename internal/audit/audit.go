// Package audit records structured gateway evidence.
package audit

import (
	"context"
	"time"

	"llmgate/internal/llmtypes"
)

// AuthError names the failure mode at the gateway auth boundary. The
// wire response collapses every auth failure to 401, so the kind only
// lives in the audit / access-log surface. Empty value means auth
// succeeded (or the route had no auth at all).
type AuthError string

const (
	AuthErrorMissing AuthError = "missing"
	AuthErrorFormat  AuthError = "format"
	AuthErrorUnknown AuthError = "unknown"

	SchemaVersion  = 1
	EventTypeAudit = "audit"
	EventTypeCall  = "call"
)

// EventCommon holds fields shared by operational audit records and LLM
// call-result records. request_id is the join key across access / audit / call.
type EventCommon struct {
	Timestamp time.Time
	RequestID string

	// Operation is the gateway-domain method name —
	// "chat.completions" or "chat.completions.stream" — not the HTTP
	// method. Naming it Method would clash with r.Method (HTTP "POST")
	// in mixed log streams.
	Operation string

	// ConsumerName identifies the registered caller (matched yaml `name` in
	// consumers/) for this request. Empty when the request was rejected at
	// the auth boundary; the record is still emitted in that case so
	// brute-force / mis-configured-key activity is observable. ConsumerKeyID
	// is the first 8 hex chars of the matched hash (sha256), useful for
	// detecting which key in a rotating set was actually used.
	ConsumerName  string
	ConsumerKeyID string

	StatusCode int
	Kind       llmtypes.ErrorKind
	DurationMS int64
}

// Record captures the per-request operational audit payload. It is emitted
// for every chat request, including auth failures and panics. LLM invocation
// details live in CallRecord so billing / analytics consumers can move to a
// message stream without coupling to the operational audit stream.
type Record struct {
	EventCommon
	AuthError AuthError
}

// Recorder receives one operational Record per gateway request.
type Recorder interface {
	RecordAudit(ctx context.Context, r *Record)
	Close() error
}

// Nop drops every record. Default when no recorder is configured.
type Nop struct{}

func (Nop) RecordAudit(context.Context, *Record) {}
func (Nop) Close() error                         { return nil }
