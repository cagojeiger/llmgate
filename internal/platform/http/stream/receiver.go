package stream

import (
	"context"
	"errors"
	"sync"
	"time"

	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/streaming"
)

type recvResult struct {
	event *llmtypes.Event
	err   error
}

type streamReceiver struct {
	stream   llmtypes.Stream
	requests chan struct{}
	results  chan recvResult
	stopOnce sync.Once
}

func newStreamReceiver(stream llmtypes.Stream) *streamReceiver {
	r := &streamReceiver{
		stream:   stream,
		requests: make(chan struct{}),
		results:  make(chan recvResult, 1),
	}
	go r.run()
	return r
}

func (r *streamReceiver) run() {
	for range r.requests {
		event, err := r.stream.Recv()
		r.results <- recvResult{event: event, err: err}
		if err != nil {
			return
		}
	}
}

func (r *streamReceiver) Stop() {
	r.stopOnce.Do(func() {
		close(r.requests)
	})
}

// Recv pulls one event from stream, bounded by the idle timeout (no event
// between Recv calls). The worker goroutine is reused for the whole stream,
// while Run still requests only one Recv after each downstream write.
func (r *streamReceiver) Recv(ctx context.Context, timeout time.Duration) (*llmtypes.Event, error) {
	select {
	case r.requests <- struct{}{}:
	case <-ctx.Done():
		_ = r.stream.Close()
		return nil, streamContextError(ctx.Err())
	}

	var timeoutC <-chan time.Time
	var timer *time.Timer
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		defer timer.Stop()
		timeoutC = timer.C
	}

	select {
	case got := <-r.results:
		return got.event, got.err
	case <-timeoutC:
		_ = r.stream.Close()
		streaming.DrainRecvOrAbandon(r.results, streaming.CloseGrace)
		return nil, &llmtypes.Error{Kind: llmtypes.KindTimeout, Message: "stream idle timeout"}
	case <-ctx.Done():
		_ = r.stream.Close()
		streaming.DrainRecvOrAbandon(r.results, streaming.CloseGrace)
		return nil, streamContextError(ctx.Err())
	}
}

func streamContextError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &llmtypes.Error{Kind: llmtypes.KindTimeout, Message: err.Error(), Cause: err}
	}
	return err
}
