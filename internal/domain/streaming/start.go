package streaming

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime/debug"

	"llmgate/internal/domain/llmtypes"
)

// ValidateStreamStart eagerly reads one event from raw to confirm the
// stream is alive, then returns a Stream that replays that event on its
// first Recv() call. Adapters call this at the end of CompleteStream so
// pre-first-event failures surface as a CompleteStream error — enabling
// service-level fallback without the Service needing its own
// eager-read.
//
// Lifecycle:
//   - On Recv error or ctx cancel: raw is closed, the error is returned,
//     and the goroutine is bounded by CloseGrace.
//   - On success: caller owns Close() of the returned stream, which
//     forwards to raw.Close().
func ValidateStreamStart(ctx context.Context, raw llmtypes.Stream) (llmtypes.Stream, error) {
	type result struct {
		event *llmtypes.Event
		err   error
	}
	ch := make(chan result, 1)
	// Recover defer below intentionally logs via a fresh context — the
	// parent ctx may have fired by the time the goroutine panics.
	go func() { //nolint:contextcheck // process-detached recover ctx
		defer func() {
			if p := recover(); p != nil {
				slog.Default().LogAttrs(context.Background(), slog.LevelError,
					"stream start worker panic",
					slog.Any("panic", p),
					slog.String("stack", string(debug.Stack())),
				)
				// Non-blocking: ch is capacity 1 and empty at this point.
				select {
				case ch <- result{err: &llmtypes.Error{
					Kind:    llmtypes.KindPanic,
					Message: fmt.Sprintf("stream start panic: %v", p),
				}}:
				default:
				}
			}
		}()
		event, err := raw.Recv()
		ch <- result{event, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			_ = raw.Close()
			if errors.Is(r.err, io.EOF) {
				return nil, &llmtypes.Error{Kind: llmtypes.KindUpstream, Message: "stream ended before first event", Cause: r.err}
			}
			return nil, r.err
		}
		// Race guard: the first event arrived but ctx (e.g. a stream-start
		// timer) may have fired in the same instant. Promote that to a
		// failure so the caller can fall back rather than handing the
		// client a stream whose underlying ctx is already canceled.
		if err := ctx.Err(); err != nil {
			_ = raw.Close()
			return nil, err
		}
		return &replayStream{first: r.event, underlying: raw}, nil
	case <-ctx.Done():
		_ = raw.Close()
		// Bool return is discarded here: the caller's ctx was already
		// cancelled (timeout or client disconnect) so the outer log
		// path has the abandon's root cause. We just need the grace
		// window so the per-request first-event goroutine doesn't
		// strand the buffered channel.
		_ = DrainRecvOrAbandon(ch, CloseGrace)
		return nil, ctx.Err()
	}
}

// replayStream wraps a Stream so the first Recv returns the event
// already eager-read during validation; subsequent calls pass through
// to the underlying stream. Close and Summary delegate.
type replayStream struct {
	first      *llmtypes.Event
	consumed   bool
	underlying llmtypes.Stream
}

func (s *replayStream) Recv() (*llmtypes.Event, error) {
	if !s.consumed {
		s.consumed = true
		return s.first, nil
	}
	return s.underlying.Recv()
}

func (s *replayStream) Close() error {
	return s.underlying.Close()
}

func (s *replayStream) Summary() *llmtypes.Summary {
	return s.underlying.Summary()
}
