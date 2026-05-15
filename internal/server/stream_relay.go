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
	"llmgate/internal/llmtypes"
	"llmgate/internal/streaming"
)

type streamRelay struct {
	log         *slog.Logger
	idleTimeout time.Duration
}

func newStreamRelay(log *slog.Logger, idleTimeout time.Duration) *streamRelay {
	return &streamRelay{log: log, idleTimeout: idleTimeout}
}

func (s *streamRelay) Run(ctx context.Context, w http.ResponseWriter, stream llmtypes.Stream, rec *audit.Record, call *audit.CallRecord) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		perr := &llmtypes.Error{Kind: llmtypes.KindUnknown, Message: "streaming unsupported"}
		adoptError(rec, perr)
		writeError(w, perr)
		return
	}

	sink := newSSEWriter(w, flusher)
	defer func() { call.ResponseBytes = sink.Bytes() }()
	sink.WriteHeaders()
	rec.StatusCode = http.StatusOK

	for {
		event, err := recvWithIdleTimeout(ctx, stream, s.idleTimeout)
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
	}
}

func (s *streamRelay) recordClientClosed(ctx context.Context, rec *audit.Record, call *audit.CallRecord, werr error) {
	rec.Kind = llmtypes.KindClientClosed
	call.Kind = rec.Kind
	s.log.LogAttrs(ctx, slog.LevelInfo, "client disconnected mid-stream",
		slog.String("err", werr.Error()),
	)
}

type recvResult struct {
	event *llmtypes.Event
	err   error
}

func recvWithIdleTimeout(ctx context.Context, stream llmtypes.Stream, timeout time.Duration) (*llmtypes.Event, error) {
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
		return nil, &llmtypes.Error{Kind: llmtypes.KindTimeout, Message: "stream idle timeout"}
	case <-ctx.Done():
		_ = stream.Close()
		streaming.DrainRecvOrAbandon(ch, streaming.CloseGrace)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, &llmtypes.Error{Kind: llmtypes.KindTimeout, Message: ctx.Err().Error(), Cause: ctx.Err()}
		}
		return nil, ctx.Err()
	}
}
