package streaming

import (
	"io"
	"sync"
	"time"
)

// CloseGrace bounds how long a caller waits for a Stream.Recv goroutine to
// exit after Close() before abandoning it. Adapters are required by the
// Stream contract to unblock Recv promptly on Close; this var is the
// defensive ceiling for misbehaving adapters that violate the contract.
// It is a var (not a const) so tests can override it for speed.
var CloseGrace = 5 * time.Second

// DrainRecvOrAbandon waits up to grace for ch to deliver a result, or returns.
// The caller's buffered channel keeps the abandoned Recv goroutine from
// leaking the channel itself; the goroutine is left to complete in the
// background if the adapter never honors Close. Returns true if the
// channel delivered before the grace expired (clean), false if the
// goroutine was abandoned (operator-visible signal that an adapter
// did not unblock Recv promptly on Close).
func DrainRecvOrAbandon[T any](ch <-chan T, grace time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(grace):
		return false
	}
}

// StreamBase is the close + chunk-telemetry + provider-name boilerplate
// every Stream implementation needs. Adapters embed it so vendor-specific
// stream code only owns the per-event state machine — Body lifecycle,
// idempotent Close, and FirstByteAt/ChunkCount tracking are uniform.
//
// The SSE reader is *not* held here because domain streaming should not depend
// on platform/upstream. Each adapter constructs its own reader
// from the same Body it hands to StreamBase.
type StreamBase struct {
	Body         io.Closer
	ProviderName string

	closeOnce sync.Once
	closeErr  error

	// Telemetry — populated by RecordEmit() called after each event emit.
	ChunkCount  int
	FirstByteAt time.Time
}

// RecordEmit advances ChunkCount and stamps FirstByteAt on the first
// call. Embedding streams call this on each successful Event return.
func (s *StreamBase) RecordEmit() {
	if s.FirstByteAt.IsZero() {
		s.FirstByteAt = time.Now()
	}
	s.ChunkCount++
}

// Close closes the underlying body exactly once; subsequent calls return
// the cached error. Safe under concurrent callers via sync.Once.
func (s *StreamBase) Close() error {
	s.closeOnce.Do(func() {
		if s.Body != nil {
			s.closeErr = s.Body.Close()
		}
	})
	return s.closeErr
}
