package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/telemetry"
	"llmgate/internal/platform/http/response"
	"llmgate/internal/streaming"
)

// streamRelay owns the SSE wire transcript for one streaming
// request. Caller (Handler) handles the pre-stream phases (parse →
// Service → call event adoption) and the deferred Stream.Close /
// stream summary adoption; streamRelay takes over once a Stream is in
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
// fully drained or a terminal condition was reached. rec/call are mutated in
// place: StatusCode, ResponseBytes, Kind. The caller's deferred
// stream.Close() and telemetry.AdoptStreamSummary() finalize the rest.
func (s *streamRelay) Run(
	ctx context.Context,
	w http.ResponseWriter,
	stream llmtypes.Stream,
	rec *telemetry.AuditEvent,
	call *telemetry.CallEvent,
	onEvent func(*llmtypes.Event),
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		perr := &llmtypes.Error{Kind: llmtypes.KindUnknown, Message: "streaming unsupported"}
		adoptError(rec, perr)
		call.Kind = rec.Kind
		response.WriteError(w, perr)
		return
	}

	sink := response.NewSSEWriter(w, flusher)
	defer func() { call.ResponseBytes = sink.Bytes() }()
	sink.WriteHeaders()
	rec.StatusCode = http.StatusOK
	call.StatusCode = rec.StatusCode

	receiver := newStreamReceiver(stream)
	defer receiver.Stop()
	for {
		event, err := receiver.Recv(ctx, s.idleTimeout)
		if errors.Is(err, io.EOF) {
			if werr := sink.SendDone(); werr != nil {
				s.recordClientClosed(ctx, rec, call, werr)
			}
			return
		}
		if err != nil {
			if errors.Is(err, context.Canceled) {
				s.recordClientClosed(ctx, rec, call, err)
				return
			}
			k := llmtypes.ErrorKindOf(err)
			rec.Kind = k
			call.Kind = k
			s.log.LogAttrs(ctx, slog.LevelWarn, "stream receive failed",
				slog.String("vendor", call.Vendor),
				slog.String("err", err.Error()),
			)
			_ = sink.SendError(err)
			_ = sink.SendDone()
			return
		}

		payload, err := json.Marshal(event)
		if err != nil {
			perr := &llmtypes.Error{Kind: llmtypes.KindUnknown, Message: "encode stream event: " + err.Error(), Cause: err}
			rec.Kind = perr.Kind
			call.Kind = perr.Kind
			_ = sink.SendError(perr)
			_ = sink.SendDone()
			return
		}
		if werr := sink.Send(payload); werr != nil {
			s.recordClientClosed(ctx, rec, call, werr)
			return
		}
		if onEvent != nil {
			onEvent(event)
		}
	}
}

// recordClientClosed marks rec terminal state as a client disconnect.
// Caller should return immediately afterwards — further writes would
// fail the same way and SendDone would too.
func (s *streamRelay) recordClientClosed(
	ctx context.Context,
	rec *telemetry.AuditEvent,
	call *telemetry.CallEvent,
	werr error,
) {
	rec.Kind = llmtypes.KindClientClosed
	call.Kind = rec.Kind
	s.log.LogAttrs(ctx, slog.LevelInfo, "client disconnected mid-stream",
		slog.String("vendor", call.Vendor),
		slog.String("err", werr.Error()),
	)
}

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
