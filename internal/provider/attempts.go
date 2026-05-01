package provider

import (
	"context"
	"sync"
	"time"
)

// Attempt records the outcome of one upstream call inside a single
// gateway request. A non-fallback request produces exactly one Attempt;
// a fallback chain that retries N times produces N — typically N-1 with
// errors followed by one success.
//
// Usage may be nil when the upstream rejected before generation (4xx,
// pre-stream 5xx). For mid-stream truncation, adapters surface partial
// usage via Stream.Summary so the value here can be non-nil even with
// a non-success ErrorKind.
type Attempt struct {
	Vendor     string
	Model      string
	StartedAt  time.Time
	DurationMS int64
	StatusCode int
	ErrorKind  Kind
	Usage      *Usage
	VendorCost string
}

// attemptCtxKey is unexported so callers must use the helpers below; this
// keeps the side-channel discoverable via `git grep` instead of any
// `ctx.Value(...)` site spreading silently.
type attemptCtxKey struct{}

type attemptHolder struct {
	mu       sync.Mutex
	attempts []Attempt
}

// WithAttemptHolder returns a context that accumulates Attempts recorded
// during downstream Provider calls. The handler installs the holder
// before invoking the Provider chain; the router (or any other Provider
// in the chain) calls RecordAttempt to push entries; the handler reads
// AttemptsFromContext after the chain returns.
//
// If the holder is absent, RecordAttempt and AttemptsFromContext are
// no-ops, so single-attempt callers (probe CLI, tests) work unchanged.
func WithAttemptHolder(ctx context.Context) context.Context {
	return context.WithValue(ctx, attemptCtxKey{}, &attemptHolder{})
}

// RecordAttempt appends an Attempt to the holder installed by
// WithAttemptHolder. Concurrent-safe; safe to call without a holder
// (returns without doing anything).
func RecordAttempt(ctx context.Context, a Attempt) {
	h, ok := ctx.Value(attemptCtxKey{}).(*attemptHolder)
	if !ok {
		return
	}
	h.mu.Lock()
	h.attempts = append(h.attempts, a)
	h.mu.Unlock()
}

// AttemptsFromContext returns a copy of the recorded Attempts. Returns
// nil when no holder is installed.
func AttemptsFromContext(ctx context.Context) []Attempt {
	h, ok := ctx.Value(attemptCtxKey{}).(*attemptHolder)
	if !ok {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.attempts) == 0 {
		return nil
	}
	out := make([]Attempt, len(h.attempts))
	copy(out, h.attempts)
	return out
}
