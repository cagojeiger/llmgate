package llmtypes

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type ErrorKind string

const (
	KindAuth          ErrorKind = "auth"
	KindRateLimit     ErrorKind = "rate_limit"
	KindBadRequest    ErrorKind = "bad_request"
	KindContextLength ErrorKind = "context_length"
	KindContentFilter ErrorKind = "content_filter"
	KindUpstream      ErrorKind = "upstream"
	KindTimeout       ErrorKind = "timeout"
	KindNetwork       ErrorKind = "network"
	KindEmpty         ErrorKind = "empty_response"
	KindClientClosed  ErrorKind = "client_closed"
	KindPanic         ErrorKind = "panic"
	KindUnknown       ErrorKind = "unknown"
)

type Error struct {
	Kind       ErrorKind
	Provider   string
	Message    string
	StatusCode int
	RetryAfter time.Duration
	Cause      error
	Raw        []byte
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Provider != "" {
		return fmt.Sprintf("%s/%s: %s", e.Provider, e.Kind, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }

func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	if t.Kind == "" {
		return false
	}
	return e.Kind == t.Kind
}

// ErrorKindOf extracts the gateway error kind from err. It is the common
// read-side helper for routing policy, server presentation, and audit
// stamping so each layer does not repeat llmtypes.Error unwrapping.
func ErrorKindOf(err error) ErrorKind {
	if err == nil {
		return ""
	}
	var perr *Error
	if errors.As(err, &perr) && perr.Kind != "" {
		return perr.Kind
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return KindTimeout
	}
	return KindUnknown
}

// StatusCodeOf returns the upstream status preserved on a llmtypes.Error,
// or 0 when err is not provider-shaped or the upstream did not provide one.
func StatusCodeOf(err error) int {
	var perr *Error
	if errors.As(err, &perr) {
		return perr.StatusCode
	}
	return 0
}

// RetryAfterOf returns the retry hint preserved on a llmtypes.Error.
func RetryAfterOf(err error) time.Duration {
	var perr *Error
	if errors.As(err, &perr) {
		return perr.RetryAfter
	}
	return 0
}

// CauseOf returns the wrapped Cause of the chain-found *Error, or nil
// if the chain has no *Error or its Cause is nil. Pairs with the other
// *Of helpers so downstream layers don't repeat type asserts when they
// need to distinguish errors that wrap an underlying error (always true
// for low-level transport faults built via upstream/http.go's
// LowLevelError) from errors built without one (true for adapter
// classified diagnostics like "empty response" or HTTP 408 envelopes).
// Works through fmt.Errorf("...: %w", err) wrappers, so a re-wrapped
// adapter error is still recognized as adapter-origin.
func CauseOf(err error) error {
	var perr *Error
	if errors.As(err, &perr) {
		return perr.Cause
	}
	return nil
}

// MessageOf returns the provider-facing message, falling back to err.Error.
func MessageOf(err error) string {
	var perr *Error
	if errors.As(err, &perr) {
		return perr.Message
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

// StampProvider attaches name to err's *Error.Provider when missing,
// so call sites in adapter packages don't repeat the same wrap helper.
// Pass-through for non-*Error errors.
func StampProvider(err error, name string) error {
	var perr *Error
	if !errors.As(err, &perr) {
		return err
	}
	if perr.Provider == name {
		return perr
	}
	stamped := *perr
	stamped.Provider = name
	return &stamped
}
