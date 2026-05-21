package chat

import (
	"net/http"
	"time"

	llmresultassembly "llmgate/internal/domain/llmresult/assembly"
	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/telemetry"
	"llmgate/internal/platform/http/response"
)

func (h *Handler) serveStream(w http.ResponseWriter, r *http.Request, req *llmtypes.Request, rec *telemetry.AuditEvent, call *telemetry.CallEvent) *llmtypes.Response {
	result, err := h.service.CompleteStream(r.Context(), req)
	telemetry.AdoptRouteResult(call, result)
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

	builder := llmresultassembly.NewStreamResponseBuilder()
	h.stream.Run(r.Context(), w, stream, rec, call, builder.Add)
	if rec.Kind != "" {
		return nil
	}
	return builder.Response()
}
