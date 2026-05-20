// Package telemetry records structured gateway evidence.
package telemetry

import (
	"llmgate/internal/domain/llmtypes"
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

// AuthResult is the normalized authentication outcome for audit queries.
type AuthResult string

const (
	AuthResultSuccess AuthResult = "success"
	AuthResultFailure AuthResult = "failure"
)

// PolicyResult is the normalized gateway policy decision for audit queries.
type PolicyResult string

const (
	PolicyResultAllowed PolicyResult = "allowed"
	PolicyResultDenied  PolicyResult = "denied"
)

// DenyReason names why a gateway policy denied the request.
type DenyReason string

const (
	DenyReasonAuth            DenyReason = "auth"
	DenyReasonModelNotAllowed DenyReason = "model_not_allowed"
)

// AuditEvent captures the per-request operational audit payload. It is emitted
// for every chat request, including auth failures and panics. LLM invocation
// details live in CallEvent so billing / analytics consumers can move to a
// message stream without coupling to the operational audit stream.
type AuditEvent struct {
	EventCommon
	AuthResult   AuthResult
	AuthError    AuthError
	PolicyResult PolicyResult
	DenyReason   DenyReason
	ResourceType string
	ResourceID   string
}

func NewAuditEvent(common EventCommon) *AuditEvent {
	return &AuditEvent{EventCommon: common}
}

func (*AuditEvent) TelemetryEventType() string { return EventTypeAudit }

func FinishAuditEvent(rec *AuditEvent, statusCode int, kind llmtypes.ErrorKind, durationMS int64) {
	rec.StatusCode = statusCode
	rec.Kind = kind
	rec.DurationMS = durationMS
}

func MarkAuthFailure(rec *AuditEvent, authErr AuthError) {
	if rec == nil {
		return
	}
	rec.AuthResult = AuthResultFailure
	rec.AuthError = authErr
	rec.PolicyResult = PolicyResultDenied
	rec.DenyReason = DenyReasonAuth
}

func MarkAuthSuccess(rec *AuditEvent) {
	if rec != nil {
		rec.AuthResult = AuthResultSuccess
	}
}

func SetResource(rec *AuditEvent, resourceType, resourceID string) {
	if rec == nil {
		return
	}
	rec.ResourceType = resourceType
	rec.ResourceID = resourceID
}

func MarkPolicyAllowed(rec *AuditEvent) {
	if rec != nil {
		rec.PolicyResult = PolicyResultAllowed
	}
}

func MarkPolicyDenied(rec *AuditEvent, reason DenyReason) {
	if rec == nil {
		return
	}
	rec.PolicyResult = PolicyResultDenied
	rec.DenyReason = reason
}
