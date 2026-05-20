package sink

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"llmgate/internal/events/llmresult"
)

func TestAsyncSink_EmitDoesNotWaitForTransport(t *testing.T) {
	next := newBlockingResultSink()
	sink := NewAsyncSinkWithConfig(next, discardLogger(), AsyncConfig{
		QueueSize:     1,
		BatchSize:     1,
		FlushInterval: time.Hour,
	})
	defer sink.Close()
	defer next.release()

	sink.Emit(context.Background(), &llmresult.Event{RequestID: "req-1"})
	next.waitStarted(t)

	start := time.Now()
	sink.Emit(context.Background(), &llmresult.Event{RequestID: "req-2"})
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("Emit blocked for %v, want non-blocking enqueue", elapsed)
	}
}

func TestAsyncSink_DropsWhenQueueFull(t *testing.T) {
	next := newBlockingResultSink()
	sink := NewAsyncSinkWithConfig(next, discardLogger(), AsyncConfig{
		QueueSize:     1,
		BatchSize:     1,
		FlushInterval: time.Hour,
	})
	defer sink.Close()
	defer next.release()

	sink.Emit(context.Background(), &llmresult.Event{RequestID: "req-1"})
	next.waitStarted(t)
	sink.Emit(context.Background(), &llmresult.Event{RequestID: "req-2"})
	sink.Emit(context.Background(), &llmresult.Event{RequestID: "req-3"})

	if got := sink.Dropped(); got != 1 {
		t.Fatalf("Dropped = %d, want 1", got)
	}
}

func TestAsyncSink_FlushesWhenBatchSizeReached(t *testing.T) {
	next := &captureResultSink{}
	sink := NewAsyncSinkWithConfig(next, discardLogger(), AsyncConfig{
		QueueSize:     10,
		BatchSize:     2,
		FlushInterval: time.Hour,
	})
	defer sink.Close()

	sink.Emit(context.Background(), &llmresult.Event{RequestID: "req-1"})
	assertEventuallyLen(t, next, 0)

	sink.Emit(context.Background(), &llmresult.Event{RequestID: "req-2"})
	waitLen(t, next, 2)
}

func TestAsyncSink_FlushesWhenIntervalElapses(t *testing.T) {
	next := &captureResultSink{}
	sink := NewAsyncSinkWithConfig(next, discardLogger(), AsyncConfig{
		QueueSize:     10,
		BatchSize:     100,
		FlushInterval: 10 * time.Millisecond,
	})
	defer sink.Close()

	sink.Emit(context.Background(), &llmresult.Event{RequestID: "req-1"})

	waitLen(t, next, 1)
}

func TestAsyncSink_CloseDrainsQueueThenClosesNext(t *testing.T) {
	next := &captureResultSink{}
	sink := NewAsyncSinkWithConfig(next, discardLogger(), AsyncConfig{
		QueueSize:     10,
		BatchSize:     100,
		FlushInterval: time.Hour,
	})

	sink.Emit(context.Background(), &llmresult.Event{RequestID: "req-1"})
	sink.Emit(context.Background(), &llmresult.Event{RequestID: "req-2"})

	if err := sink.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got := next.len(); got != 2 {
		t.Fatalf("captured events = %d, want 2", got)
	}
	if !next.closed {
		t.Fatal("next sink was not closed")
	}
}

func TestAsyncSink_CloseReturnsNextCloseError(t *testing.T) {
	want := errors.New("close failed")
	sink := NewAsyncSink(&closeErrorSink{err: want}, discardLogger(), 1)

	if got := sink.Close(); !errors.Is(got, want) {
		t.Fatalf("Close() error = %v, want %v", got, want)
	}
}

func TestAsyncSink_CloseIsIdempotent(t *testing.T) {
	next := &captureResultSink{}
	sink := NewAsyncSinkWithConfig(next, discardLogger(), AsyncConfig{
		QueueSize:     1,
		BatchSize:     1,
		FlushInterval: time.Hour,
	})

	if err := sink.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if got := next.closeCount(); got != 1 {
		t.Fatalf("next Close calls = %d, want 1", got)
	}
}

func TestAsyncSink_RecoversWorkerPanic(t *testing.T) {
	next := &panicOnceResultSink{}
	sink := NewAsyncSinkWithConfig(next, discardLogger(), AsyncConfig{
		QueueSize:     2,
		BatchSize:     2,
		FlushInterval: time.Hour,
	})

	sink.Emit(context.Background(), &llmresult.Event{RequestID: "req-1"})
	sink.Emit(context.Background(), &llmresult.Event{RequestID: "req-2"})

	if err := sink.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got := next.len(); got != 1 {
		t.Fatalf("captured events after panic = %d, want 1", got)
	}
}

type blockingResultSink struct {
	started  chan struct{}
	releasec chan struct{}
	once     sync.Once
}

func newBlockingResultSink() *blockingResultSink {
	return &blockingResultSink{
		started:  make(chan struct{}),
		releasec: make(chan struct{}),
	}
}

func (s *blockingResultSink) Emit(context.Context, *llmresult.Event) {
	s.once.Do(func() { close(s.started) })
	<-s.releasec
}

func (s *blockingResultSink) Close() error { return nil }

func (s *blockingResultSink) release() {
	close(s.releasec)
}

func (s *blockingResultSink) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-s.started:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for blocking sink")
	}
}

type captureResultSink struct {
	mu     sync.Mutex
	events []*llmresult.Event
	closed bool
	closes int
}

func (s *captureResultSink) Emit(_ context.Context, event *llmresult.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func (s *captureResultSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.closes++
	return nil
}

func (s *captureResultSink) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

func (s *captureResultSink) closeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closes
}

func waitLen(t *testing.T, sink *captureResultSink, want int) {
	t.Helper()
	deadline := time.After(500 * time.Millisecond)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if got := sink.len(); got == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("captured events = %d, want %d", sink.len(), want)
		case <-ticker.C:
		}
	}
}

func assertEventuallyLen(t *testing.T, sink *captureResultSink, want int) {
	t.Helper()
	time.Sleep(20 * time.Millisecond)
	if got := sink.len(); got != want {
		t.Fatalf("captured events = %d, want %d", got, want)
	}
}

type closeErrorSink struct {
	err error
}

func (s *closeErrorSink) Emit(context.Context, *llmresult.Event) {}
func (s *closeErrorSink) Close() error                           { return s.err }

type panicOnceResultSink struct {
	mu      sync.Mutex
	events  []*llmresult.Event
	paniced bool
}

func (s *panicOnceResultSink) Emit(_ context.Context, event *llmresult.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.paniced {
		s.paniced = true
		panic("boom")
	}
	s.events = append(s.events, event)
}

func (s *panicOnceResultSink) Close() error { return nil }

func (s *panicOnceResultSink) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
