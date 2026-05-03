package provider

import (
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
