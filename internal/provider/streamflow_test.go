package provider

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

// stubbornStream simulates a misbehaving adapter: Recv blocks forever
// and Close does not unblock it. Used to verify ValidateFirstEvent's
// goroutine-leak safety net (DrainOrAbandon with CloseGrace).
type stubbornStream struct {
	closeCalled int
	block       chan struct{}
}

func newStubbornStream() *stubbornStream {
	return &stubbornStream{block: make(chan struct{})}
}

func (s *stubbornStream) Recv() (*Event, error) {
	<-s.block
	return nil, io.EOF
}

func (s *stubbornStream) Close() error {
	s.closeCalled++
	return nil
}

func (s *stubbornStream) Summary() *Summary { return &Summary{} }

func (s *stubbornStream) release() { close(s.block) }

func TestValidateFirstEvent_BoundedDrainOnContractViolation(t *testing.T) {
	prev := CloseGrace
	CloseGrace = 50 * time.Millisecond
	defer func() { CloseGrace = prev }()

	s := newStubbornStream()
	defer s.release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	_, err := ValidateFirstEvent(ctx, s)
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Fatalf("ValidateFirstEvent returned in %v, want < 500ms (grace=50ms) — leak guard not engaged", elapsed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if s.closeCalled == 0 {
		t.Errorf("Stream.Close() not invoked before bounded wait")
	}
}
