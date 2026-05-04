package provider

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type Kind string

const (
	KindAuth          Kind = "auth"
	KindRateLimit     Kind = "rate_limit"
	KindBadRequest    Kind = "bad_request"
	KindContextLength Kind = "context_length"
	KindContentFilter Kind = "content_filter"
	KindUpstream      Kind = "upstream"
	KindTimeout       Kind = "timeout"
	KindNetwork       Kind = "network"
	KindEmpty         Kind = "empty_response"
	KindClientClosed  Kind = "client_closed"
	KindUnknown       Kind = "unknown"
)

type Error struct {
	Kind       Kind
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

// KindOf extracts the gateway error kind from err. It is the common
// read-side helper for dispatch policy, server presentation, and audit
// stamping so each layer does not repeat provider.Error unwrapping.
func KindOf(err error) Kind {
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

// StatusCodeOf returns the upstream status preserved on a provider.Error,
// or 0 when err is not provider-shaped or the upstream did not provide one.
func StatusCodeOf(err error) int {
	var perr *Error
	if errors.As(err, &perr) {
		return perr.StatusCode
	}
	return 0
}

// RetryAfterOf returns the retry hint preserved on a provider.Error.
func RetryAfterOf(err error) time.Duration {
	var perr *Error
	if errors.As(err, &perr) {
		return perr.RetryAfter
	}
	return 0
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
