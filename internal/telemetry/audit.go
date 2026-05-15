// Package telemetry records structured gateway evidence.
package telemetry

import (
	"context"

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
)

// AuditEvent captures the per-request operational audit payload. It is emitted
// for every chat request, including auth failures and panics. LLM invocation
// details live in CallEvent so billing / analytics consumers can move to a
// message stream without coupling to the operational audit stream.
type AuditEvent struct {
	EventCommon
	AuthError AuthError
}

func NewAuditEvent(common EventCommon) *AuditEvent {
	return &AuditEvent{EventCommon: common}
}

func FinishAuditEvent(rec *AuditEvent, statusCode int, kind llmtypes.ErrorKind, durationMS int64) {
	rec.StatusCode = statusCode
	rec.Kind = kind
	rec.DurationMS = durationMS
}

// AuditRecorder receives one operational AuditEvent per gateway request.
type AuditRecorder interface {
	RecordAudit(ctx context.Context, r *AuditEvent)
	Close() error
}

// NopAuditRecorder drops every record. Default when no recorder is configured.
type NopAuditRecorder struct{}

func (NopAuditRecorder) RecordAudit(context.Context, *AuditEvent) {}
func (NopAuditRecorder) Close() error                             { return nil }
