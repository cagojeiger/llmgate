package server

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/streaming"
)

// stubbornStream simulates a misbehaving adapter whose Close does not
// unblock a pending Recv. Used to verify streamReceiver's bounded wait
// safety net.
type stubbornStream struct {
	closeCalled int32
	block       chan struct{}
}

func newStubbornStream() *stubbornStream {
	return &stubbornStream{block: make(chan struct{})}
}

func (s *stubbornStream) Recv() (*llmtypes.Event, error) {
	<-s.block
	return nil, io.EOF
}

func (s *stubbornStream) Close() error {
	s.closeCalled++
	return nil
}

func (s *stubbornStream) Summary() *llmtypes.Summary { return &llmtypes.Summary{} }

func (s *stubbornStream) release() { close(s.block) }

func TestStreamReceiver_RequestsOneRecvAtATime(t *testing.T) {
	s := newCountingStream(
		streamEvent("first"),
		streamEvent("second"),
	)
	receiver := newStreamReceiver(s)
	defer receiver.Stop()

	event, err := receiver.Recv(context.Background(), 0)
	if err != nil {
		t.Fatalf("first Recv() error = %v", err)
	}
	if got := event.Choices[0].Delta.Content; got != "first" {
		t.Fatalf("first event content = %q, want first", got)
	}
	s.mustObserveRecv(t, 1)
	s.mustNotObserveRecv(t)

	event, err = receiver.Recv(context.Background(), 0)
	if err != nil {
		t.Fatalf("second Recv() error = %v", err)
	}
	if got := event.Choices[0].Delta.Content; got != "second" {
		t.Fatalf("second event content = %q, want second", got)
	}
	s.mustObserveRecv(t, 2)
}

type countingStream struct {
	mu          sync.Mutex
	events      []*llmtypes.Event
	cursor      int
	recvStarted chan int
}

func newCountingStream(events ...*llmtypes.Event) *countingStream {
	return &countingStream{
		events:      events,
		recvStarted: make(chan int, len(events)+1),
	}
}

func streamEvent(content string) *llmtypes.Event {
	return &llmtypes.Event{
		Choices: []llmtypes.ChoiceDelta{{
			Delta: llmtypes.Delta{Content: content},
		}},
	}
}

func (s *countingStream) Recv() (*llmtypes.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recvStarted <- s.cursor + 1
	if s.cursor >= len(s.events) {
		return nil, io.EOF
	}
	event := s.events[s.cursor]
	s.cursor++
	return event, nil
}

func (s *countingStream) Close() error { return nil }

func (s *countingStream) Summary() *llmtypes.Summary { return &llmtypes.Summary{} }

func (s *countingStream) mustObserveRecv(t *testing.T, want int) {
	t.Helper()
	select {
	case got := <-s.recvStarted:
		if got != want {
			t.Fatalf("Recv call = %d, want %d", got, want)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("timed out waiting for Recv call %d", want)
	}
}

func (s *countingStream) mustNotObserveRecv(t *testing.T) {
	t.Helper()
	select {
	case got := <-s.recvStarted:
		t.Fatalf("unexpected extra Recv call %d without a relay request", got)
	case <-time.After(25 * time.Millisecond):
	}
}

func TestStreamReceiver_BoundedDrainOnContextCancel(t *testing.T) {
	prev := streaming.CloseGrace
	streaming.CloseGrace = 50 * time.Millisecond
	defer func() { streaming.CloseGrace = prev }()

	s := newStubbornStream()
	defer s.release()
	receiver := newStreamReceiver(s)
	defer receiver.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	_, err := receiver.Recv(ctx, 0)
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Fatalf("streamReceiver.Recv returned in %v, want < 500ms (grace=50ms)", elapsed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if s.closeCalled == 0 {
		t.Errorf("Stream.Close() not invoked")
	}
}

func TestStreamReceiver_BoundedDrainOnIdleTimeout(t *testing.T) {
	prev := streaming.CloseGrace
	streaming.CloseGrace = 50 * time.Millisecond
	defer func() { streaming.CloseGrace = prev }()

	s := newStubbornStream()
	defer s.release()
	receiver := newStreamReceiver(s)
	defer receiver.Stop()

	start := time.Now()
	_, err := receiver.Recv(context.Background(), 20*time.Millisecond)
	elapsed := time.Since(start)

	// Idle timer fires (~20ms) -> Close -> 50ms grace -> return. Total ~70ms.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("streamReceiver.Recv returned in %v, want < 500ms", elapsed)
	}
	var perr *llmtypes.Error
	if !errors.As(err, &perr) || perr.Kind != llmtypes.KindTimeout {
		t.Errorf("err = %v, want KindTimeout llmtypes.Error", err)
	}
	if s.closeCalled == 0 {
		t.Errorf("Stream.Close() not invoked on idle timeout")
	}
}
