package streaming

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"llmgate/internal/domain/llmtypes"
)

// stubbornStream simulates a misbehaving adapter: Recv blocks forever
// and Close does not unblock it. Used to verify ValidateStreamStart's
// goroutine-leak safety net (DrainRecvOrAbandon with CloseGrace).
type stubbornStream struct {
	closeCalled int
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

func TestValidateStreamStart_BoundedDrainOnContractViolation(t *testing.T) {
	prev := CloseGrace
	CloseGrace = 50 * time.Millisecond
	defer func() { CloseGrace = prev }()

	s := newStubbornStream()
	defer s.release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	_, err := ValidateStreamStart(ctx, s)
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Fatalf("ValidateStreamStart returned in %v, want < 500ms (grace=50ms) — leak guard not engaged", elapsed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if s.closeCalled == 0 {
		t.Errorf("Stream.Close() not invoked before bounded wait")
	}
}
