package core

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestErrorKindOf(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want ErrorKind
	}{
		{"nil", nil, ""},
		{"provider kind", &Error{ErrorKind: KindRateLimit, Message: "slow"}, KindRateLimit},
		{"wrapped provider kind", errors.Join(errors.New("outer"), &Error{ErrorKind: KindAuth, Message: "bad key"}), KindAuth},
		{"deadline", context.DeadlineExceeded, KindTimeout},
		{"unknown non-provider", errors.New("boom"), KindUnknown},
		{"empty provider kind", &Error{Message: "empty kind"}, KindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ErrorKindOf(tc.err); got != tc.want {
				t.Fatalf("ErrorKindOf() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProviderErrorAccessors(t *testing.T) {
	target := &Error{
		ErrorKind:  KindRateLimit,
		Message:    "slow down",
		StatusCode: 429,
		RetryAfter: 3 * time.Second,
	}
	err := errors.Join(errors.New("outer"), target)

	if got := StatusCodeOf(err); got != 429 {
		t.Fatalf("StatusCodeOf() = %d, want 429", got)
	}
	if got := RetryAfterOf(err); got != 3*time.Second {
		t.Fatalf("RetryAfterOf() = %v, want 3s", got)
	}
	if got := MessageOf(err); got != "slow down" {
		t.Fatalf("MessageOf() = %q, want slow down", got)
	}
}

func TestProviderErrorAccessors_NonProvider(t *testing.T) {
	err := errors.New("boom")

	if got := StatusCodeOf(err); got != 0 {
		t.Fatalf("StatusCodeOf() = %d, want 0", got)
	}
	if got := RetryAfterOf(err); got != 0 {
		t.Fatalf("RetryAfterOf() = %v, want 0", got)
	}
	if got := MessageOf(err); got != "boom" {
		t.Fatalf("MessageOf() = %q, want boom", got)
	}
}
