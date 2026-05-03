package provider

import (
	"context"
	"errors"
	"io"
)

// ValidateFirstEvent eagerly reads one event from raw to confirm the
// stream is alive, then returns a Stream that replays that event on its
// first Recv() call. Adapters call this at the end of CompleteStream so
// pre-first-event failures surface as a CompleteStream error — enabling
// router-level fallback (Window-#2) without router needing its own
// eager-read.
//
// Lifecycle:
//   - On Recv error or ctx cancel: raw is closed, the error is returned,
//     and the goroutine is bounded by CloseGrace.
//   - On success: caller owns Close() of the returned stream, which
//     forwards to raw.Close().
func ValidateFirstEvent(ctx context.Context, raw Stream) (Stream, error) {
	type result struct {
		event *Event
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		event, err := raw.Recv()
		ch <- result{event, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			_ = raw.Close()
			if errors.Is(r.err, io.EOF) {
				return nil, &Error{Kind: KindUpstream, Message: "stream ended before first event", Cause: r.err}
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
		DrainOrAbandon(ch, CloseGrace)
		return nil, ctx.Err()
	}
}

// replayStream wraps a Stream so the first Recv returns the event
// already eager-read during validation; subsequent calls pass through
// to the underlying stream. Close and Summary delegate.
type replayStream struct {
	first      *Event
	consumed   bool
	underlying Stream
}

func (s *replayStream) Recv() (*Event, error) {
	if !s.consumed {
		s.consumed = true
		return s.first, nil
	}
	return s.underlying.Recv()
}

func (s *replayStream) Close() error {
	return s.underlying.Close()
}

func (s *replayStream) Summary() *Summary {
	return s.underlying.Summary()
}
