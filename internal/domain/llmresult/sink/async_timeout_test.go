package sink

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	llmresult "llmgate/internal/domain/llmresult/schema"
)

// TestAsyncSink_Close_TimeoutAbandonsHangingWorker verifies that Close()
// returns within CloseTimeout even if the worker goroutine is stuck
// inside next.Emit. The hanging worker continues in the background; the
// caller proceeds to next.Close so the shutdown sequence can finish.
func TestAsyncSink_Close_TimeoutAbandonsHangingWorker(t *testing.T) {
	next := newBlockingResultSink()
	defer next.release()

	sink := NewAsyncSinkWithConfig(next, discardLogger(), AsyncConfig{
		QueueSize:     1,
		BatchSize:     1,
		FlushInterval: time.Hour,
		CloseTimeout:  50 * time.Millisecond,
	})

	sink.Emit(context.Background(), &llmresult.Event{RequestID: "stuck"})
	next.waitStarted(t)

	done := make(chan error, 1)
	go func() { done <- sink.Close() }()

	select {
	case <-done:
		// expected — Close returned despite hanging worker
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return within CloseTimeout; worker abandonment broken")
	}
}

// ctxRespectingSlowSink blocks inside Emit until ctx is cancelled. This
// models a NATS publisher that respects the per-emit timeout.
type ctxRespectingSlowSink struct {
	emits    atomic.Int64
	ctxDones atomic.Int64
	mu       sync.Mutex
	closed   bool
}

func (s *ctxRespectingSlowSink) Emit(ctx context.Context, _ *llmresult.Event) {
	s.emits.Add(1)
	<-ctx.Done()
	s.ctxDones.Add(1)
}

func (s *ctxRespectingSlowSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// TestAsyncSink_Emit_RespectsTimeoutAndDrainsQueue verifies that a slow
// downstream sink does not freeze the drain loop: each event hits its
// EmitTimeout, ctx fires, the worker moves to the next event, and the
// queue keeps progressing.
func TestAsyncSink_Emit_RespectsTimeoutAndDrainsQueue(t *testing.T) {
	next := &ctxRespectingSlowSink{}
	sink := NewAsyncSinkWithConfig(next, discardLogger(), AsyncConfig{
		QueueSize:     8,
		BatchSize:     2,
		FlushInterval: 10 * time.Millisecond,
		EmitTimeout:   30 * time.Millisecond,
		CloseTimeout:  2 * time.Second,
	})

	const want = 4
	for i := 0; i < want; i++ {
		sink.Emit(context.Background(), &llmresult.Event{RequestID: "e"})
	}

	if err := sink.Close(); err != nil {
		t.Fatalf("Close error = %v", err)
	}

	if got := next.emits.Load(); got != want {
		t.Errorf("emit attempts = %d, want %d", got, want)
	}
	if got := next.ctxDones.Load(); got != want {
		t.Errorf("ctx-done unblocks = %d, want %d (worker did not respect EmitTimeout)", got, want)
	}
}
