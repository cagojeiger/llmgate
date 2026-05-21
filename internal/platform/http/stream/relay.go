package stream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/telemetry"
	"llmgate/internal/platform/http/response"
)

// Relay owns the SSE wire transcript for one streaming
// request. Caller (Handler) handles the pre-stream phases (parse →
// Service → call event adoption) and the deferred Stream.Close /
// stream summary adoption; Relay takes over once a Stream is in
// hand and runs the Recv loop until terminal state — EOF / idle
// timeout / client disconnect / mid-stream provider error / encode
// failure — translating each into the right SSE wire pattern and
// audit fields.
type Relay struct {
	log         *slog.Logger
	idleTimeout time.Duration
}

func NewRelay(log *slog.Logger, idleTimeout time.Duration) *Relay {
	return &Relay{log: log, idleTimeout: idleTimeout}
}

// Run drives the SSE wire response. Returns when the stream has been
// fully drained or a terminal condition was reached. rec/call are mutated in
// place: StatusCode, ResponseBytes, Kind. The caller's deferred
// stream.Close() and telemetry.AdoptStreamSummary() finalize the rest.
func (s *Relay) Run(
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

	// Receiver worker is process/request-detached for panic-recover logging;
	// the recover path uses a fresh ctx by design.
	receiver := newStreamReceiver(stream, s.log) //nolint:contextcheck // detached recover ctx
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
func (s *Relay) recordClientClosed(
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

func adoptError(rec *telemetry.AuditEvent, err error) {
	rec.Kind = llmtypes.ErrorKindOf(err)
	rec.StatusCode = response.Status(err)
}
