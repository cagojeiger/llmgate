package audit

import (
	"context"
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
)

// Record captures the per-request operational audit payload. It is
// emitted for every request — including auth failures and panics — so
// brute-force / misconfigured-key activity is always observable.
// LLM invocation details (model, vendor, tokens, attempts) live in
// CallRecord and are emitted only when an LLM call was actually attempted.
type Record struct {
	EventCommon
	AuthError AuthError
}

// Recorder receives one Record per gateway request for the operational
// audit stream (security, auth failures, panics).
type Recorder interface {
	RecordAudit(ctx context.Context, r *Record)
	Close() error
}

// Nop drops every record. Default when no recorder is configured.
type Nop struct{}

func (Nop) RecordAudit(context.Context, *Record) {}
func (Nop) Close() error                          { return nil }
