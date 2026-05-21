package stream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
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
	log      *slog.Logger
	requests chan struct{}
	results  chan recvResult
	stopOnce sync.Once
}

func newStreamReceiver(stream llmtypes.Stream, log *slog.Logger) *streamReceiver {
	if log == nil {
		log = slog.Default()
	}
	r := &streamReceiver{
		stream:   stream,
		log:      log,
		requests: make(chan struct{}),
		results:  make(chan recvResult, 1),
	}
	go r.run()
	return r
}

func (r *streamReceiver) run() {
	defer func() {
		if p := recover(); p != nil {
			r.log.LogAttrs(context.Background(), slog.LevelError,
				"stream receiver worker panic",
				slog.Any("panic", p),
				slog.String("stack", string(debug.Stack())),
			)
			// Surface the panic as an error so the calling Recv() does
			// not block forever on r.results. The results channel has
			// capacity 1 and the request/response cadence guarantees it
			// is empty whenever stream.Recv runs, so a non-blocking send
			// is safe here.
			select {
			case r.results <- recvResult{err: &llmtypes.Error{
				Kind:    llmtypes.KindPanic,
				Message: fmt.Sprintf("stream receiver panic: %v", p),
			}}:
			default:
			}
		}
	}()
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

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case got := <-r.results:
		return got.event, got.err
	case <-timer.C:
		_ = r.stream.Close()
		if !streaming.DrainRecvOrAbandon(r.results, streaming.CloseGrace) {
			r.logAbandoned(ctx, "idle_timeout")
		}
		return nil, &llmtypes.Error{Kind: llmtypes.KindTimeout, Message: "stream idle timeout"}
	case <-ctx.Done():
		_ = r.stream.Close()
		if !streaming.DrainRecvOrAbandon(r.results, streaming.CloseGrace) {
			r.logAbandoned(ctx, "ctx_cancelled")
		}
		return nil, streamContextError(ctx.Err())
	}
}

// logAbandoned signals that the Recv() goroutine did not exit within
// CloseGrace after Close() — an upstream adapter that did not honor
// the Stream contract. The goroutine continues in the background until
// the next Recv() naturally returns; until then it holds upstream
// resources (HTTP body, parser buffers).
func (r *streamReceiver) logAbandoned(ctx context.Context, trigger string) {
	r.log.LogAttrs(ctx, slog.LevelWarn, "stream receiver recv abandoned",
		slog.String("trigger", trigger),
		slog.Duration("grace", streaming.CloseGrace),
	)
}

func streamContextError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &llmtypes.Error{Kind: llmtypes.KindTimeout, Message: err.Error(), Cause: err}
	}
	return err
}
