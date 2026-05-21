package chat

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/telemetry"
	"llmgate/internal/platform/http/response"
)

func (h *Handler) serveComplete(w http.ResponseWriter, r *http.Request, req *llmtypes.Request, rec *telemetry.AuditEvent, call *telemetry.CallEvent) *llmtypes.Response {
	result, err := h.service.Complete(r.Context(), req)
	telemetry.AdoptRouteResult(call, result)
	if err != nil {
		adoptError(rec, err)
		response.WriteError(w, err)
		return nil
	}

	out, err := json.Marshal(result.Response)
	if err != nil {
		perr := &llmtypes.Error{Kind: llmtypes.KindUnknown, Message: "encode response: " + err.Error(), Cause: err}
		adoptError(rec, perr)
		response.WriteError(w, perr)
		return nil
	}

	rec.StatusCode = http.StatusOK
	telemetry.AdoptResponse(call, result.Response, int64(len(out)))

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, werr := w.Write(out); werr != nil {
		rec.Kind = llmtypes.KindClientClosed
		telemetry.SetCallKind(call, rec.Kind)
		h.log.LogAttrs(r.Context(), slog.LevelInfo, "client write failed",
			slog.String("vendor", call.Vendor),
			slog.String("err", werr.Error()),
		)
		return nil
	}
	return result.Response
}
