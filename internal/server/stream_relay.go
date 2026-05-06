package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"llmgate/internal/audit"
	"llmgate/internal/core"
	"llmgate/internal/streaming"
)

// streamRelay owns the SSE wire transcript for one streaming
// request. Caller (Handler) handles the pre-stream phases (parse →
// gateway router → audit-route reflect) and the deferred Stream.Close /
// adoptStreamSummary; streamRelay takes over once a Stream is in
// hand and runs the Recv loop until terminal state — EOF / idle
// timeout / client disconnect / mid-stream provider error / encode
// failure — translating each into the right SSE wire pattern and
// audit fields.
type streamRelay struct {
	log         *slog.Logger
	idleTimeout time.Duration
}

func newStreamRelay(log *slog.Logger, idleTimeout time.Duration) *streamRelay {
	return &streamRelay{log: log, idleTimeout: idleTimeout}
}

// Run drives the SSE wire response. Returns when the stream has been
// fully drained or a terminal condition was reached. rec is mutated in
// place: StatusCode, ResponseBytes, ErrorKind. The caller's deferred
// stream.Close() and adoptStreamSummary() finalize the rest.
func (s *streamRelay) Run(ctx context.Context, w http.ResponseWriter, stream core.Stream, rec *audit.Record) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		perr := &core.Error{ErrorKind: core.KindUnknown, Message: "streaming unsupported"}
		adoptError(rec, perr)
		writeError(w, perr)
		return
	}

	sink := newSSEWriter(w, flusher)
	defer func() { rec.ResponseBytes = sink.Bytes() }()
	sink.WriteHeaders()
	rec.StatusCode = http.StatusOK

	for {
		event, err := recvWithIdleTimeout(ctx, stream, s.idleTimeout)
		if errors.Is(err, io.EOF) {
			if werr := sink.SendDone(); werr != nil {
				s.recordClientClosed(ctx, rec, werr)
			}
			return
		}
		if err != nil {
			if errors.Is(err, context.Canceled) {
				s.recordClientClosed(ctx, rec, err)
				return
			}
			rec.ErrorKind = core.ErrorKindOf(err)
			s.log.LogAttrs(ctx, slog.LevelWarn, "stream receive failed",
				slog.String("vendor", rec.Vendor),
				slog.String("err", err.Error()),
			)
			_ = sink.SendError(err)
			_ = sink.SendDone()
			return
		}

		payload, err := json.Marshal(event)
		if err != nil {
			perr := &core.Error{ErrorKind: core.KindUnknown, Message: "encode stream event: " + err.Error(), Cause: err}
			rec.ErrorKind = perr.ErrorKind
			_ = sink.SendError(perr)
			_ = sink.SendDone()
			return
		}
		if werr := sink.Send(payload); werr != nil {
			s.recordClientClosed(ctx, rec, werr)
			return
		}
	}
}

// recordClientClosed marks rec terminal state as a client disconnect.
// Caller should return immediately afterwards — further writes would
// fail the same way and SendDone would too.
func (s *streamRelay) recordClientClosed(ctx context.Context, rec *audit.Record, werr error) {
	rec.ErrorKind = core.KindClientClosed
	s.log.LogAttrs(ctx, slog.LevelInfo, "client disconnected mid-stream",
		slog.String("vendor", rec.Vendor),
		slog.String("err", werr.Error()),
	)
}

type recvResult struct {
	event *core.Event
	err   error
}

// recvWithIdleTimeout pulls the next event from stream, bounded by the
// idle timeout (no event between Recv calls). On timeout the stream is
// closed and a bounded grace period waits for the goroutine to exit;
// see streaming.CloseGrace for the safety net rationale.
func recvWithIdleTimeout(ctx context.Context, stream core.Stream, timeout time.Duration) (*core.Event, error) {
	ch := make(chan recvResult, 1)
	go func() {
		event, err := stream.Recv()
		ch <- recvResult{event: event, err: err}
	}()

	var timeoutC <-chan time.Time
	var timer *time.Timer
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		defer timer.Stop()
		timeoutC = timer.C
	}

	select {
	case got := <-ch:
		return got.event, got.err
	case <-timeoutC:
		_ = stream.Close()
		streaming.DrainRecvOrAbandon(ch, streaming.CloseGrace)
		return nil, &core.Error{ErrorKind: core.KindTimeout, Message: "stream idle timeout"}
	case <-ctx.Done():
		_ = stream.Close()
		streaming.DrainRecvOrAbandon(ch, streaming.CloseGrace)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, &core.Error{ErrorKind: core.KindTimeout, Message: ctx.Err().Error(), Cause: ctx.Err()}
		}
		return nil, ctx.Err()
	}
}
