package requestid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

type contextKey struct{}

// WithContext stashes the request id on ctx for downstream callers.
func WithContext(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

// FromContext returns the request id previously set via WithContext,
// or "" if none was set (e.g. inside the probe routes that skip the
// RequestID middleware).
func FromContext(ctx context.Context) string {
	id, _ := ctx.Value(contextKey{}).(string)
	return id
}

// Valid rejects inbound X-Request-Id values that would be unsafe to
// log or echo back: empty, > 128 bytes, or containing any byte
// outside printable ASCII (0x20–0x7e). The non-printable check stops
// a client from smuggling CR/LF or escape sequences into structured
// log output.
func Valid(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x20 || id[i] > 0x7e {
			return false
		}
	}
	return true
}

// New mints a fresh 128-bit random id as lowercase hex. If the OS
// RNG ever fails, fall back to a nanosecond timestamp prefixed with
// "ts-" so the function never returns "" — every code path that
// stamps a request id requires a non-empty value.
func New() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	n := time.Now().UnixNano()
	if n == 0 {
		n = 1
	}
	return fmt.Sprintf("ts-%016x", n)
}
