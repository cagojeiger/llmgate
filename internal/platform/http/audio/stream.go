package audio

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/telemetry"
	"llmgate/internal/platform/http/response"
)

// serveTranscriptionStream mirrors chat's serveStream: it opens the upstream
// transcription SSE, relays every frame to the client, and assembles the full
// transcript so the audit/result record carries the same complete payload the
// non-stream path records. On the happy (EOF) path it returns the assembled
// response; on any terminal error rec.Kind is stamped and nil is returned,
// exactly as chat does.
func (h *Handler) serveTranscriptionStream(w http.ResponseWriter, r *http.Request, req *llmtypes.TranscriptionRequest, rec *telemetry.AuditEvent, call *telemetry.CallEvent) *llmtypes.TranscriptionResponse {
	result, err := h.service.TranscribeStream(r.Context(), req)
	adoptRouteResult(call, result)
	if err != nil {
		adoptError(rec, err)
		response.WriteError(w, err)
		return nil
	}
	stream := result.Stream
	defer stream.Close()
	h.lifecycle.StreamStarted(r.Context(), call.EventCommon)
	defer h.lifecycle.StreamFinished(r.Context(), rec, call)
	defer func() { telemetry.AdoptStreamSummary(call, stream.Summary(), time.Now()) }()

	acc := &transcriptTextAccumulator{}
	h.relayTranscription(r.Context(), w, stream, rec, call, acc.add)
	if rec.Kind != "" {
		return nil
	}
	return &llmtypes.TranscriptionResponse{Text: acc.text()}
}

// relayTranscription drains the upstream stream, forwarding each raw SSE
// payload to the client as a data frame and terminating with [DONE]. It
// mirrors http/stream.Relay.Run's terminal handling (EOF, mid-stream error,
// client disconnect) but forwards the upstream frame verbatim rather than a
// re-encoded chat chunk. Cancellation flows through the request context, which
// the upstream body read honors, so no separate receiver goroutine is needed.
func (h *Handler) relayTranscription(
	ctx context.Context,
	w http.ResponseWriter,
	stream llmtypes.TranscriptionStream,
	rec *telemetry.AuditEvent,
	call *telemetry.CallEvent,
	onEvent func(*llmtypes.TranscriptionEvent),
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

	for {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			if werr := sink.SendDone(); werr != nil {
				h.recordStreamClientClosed(ctx, rec, call, werr)
			}
			return
		}
		if err != nil {
			if errors.Is(err, context.Canceled) {
				h.recordStreamClientClosed(ctx, rec, call, err)
				return
			}
			k := llmtypes.ErrorKindOf(err)
			rec.Kind = k
			call.Kind = k
			h.log.LogAttrs(ctx, slog.LevelWarn, "stream receive failed",
				slog.String("vendor", call.Vendor),
				slog.String("err", err.Error()),
			)
			_ = sink.SendError(err)
			_ = sink.SendDone()
			return
		}

		if werr := sink.Send(event.Raw); werr != nil {
			h.recordStreamClientClosed(ctx, rec, call, werr)
			return
		}
		if onEvent != nil {
			onEvent(event)
		}
	}
}

// recordStreamClientClosed marks a mid-stream client disconnect. The caller
// returns immediately after — further writes would fail the same way.
func (h *Handler) recordStreamClientClosed(ctx context.Context, rec *telemetry.AuditEvent, call *telemetry.CallEvent, werr error) {
	rec.Kind = llmtypes.KindClientClosed
	call.Kind = rec.Kind
	h.log.LogAttrs(ctx, slog.LevelInfo, "client disconnected mid-stream",
		slog.String("vendor", call.Vendor),
		slog.String("err", werr.Error()),
	)
}

// transcriptTextAccumulator assembles the final transcript from relayed
// events, parallel to how chat's StreamResponseBuilder assembles the response.
// The upstream's terminal transcript.text.done carries the authoritative full
// text; deltas are concatenated as a fallback when no done event arrives.
type transcriptTextAccumulator struct {
	deltas   strings.Builder
	doneText string
	haveDone bool
}

func (a *transcriptTextAccumulator) add(event *llmtypes.TranscriptionEvent) {
	if event == nil {
		return
	}
	switch event.Type {
	case "transcript.text.done":
		a.doneText = event.Text
		a.haveDone = true
	default: // transcript.text.delta and any incremental variant
		a.deltas.WriteString(event.Delta)
	}
}

func (a *transcriptTextAccumulator) text() string {
	if a.haveDone {
		return a.doneText
	}
	return a.deltas.String()
}
