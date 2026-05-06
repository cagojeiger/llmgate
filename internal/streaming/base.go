package streaming

import (
	"io"
	"sync"
	"time"
)

// StreamBase is the close + chunk-telemetry + provider-name boilerplate
// every Stream implementation needs. Adapters embed it so vendor-specific
// stream code only owns the per-event state machine — Body lifecycle,
// idempotent Close, and FirstByteAt/ChunkCount tracking are uniform.
//
// The SSE reader is *not* held here because streaming/ should not depend on
// upstream/. Each adapter constructs its own reader
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
