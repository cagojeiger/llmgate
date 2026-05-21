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

	// The server's WriteTimeout is 0 because the streaming path needs an
	// unbounded write window. Non-stream writes must enforce their own
	// deadline so a slow-reader client cannot keep this goroutine and the
	// in-flight response buffer alive past the request's deadline. Tie it
	// to the request context's deadline (set at handler entry from
	// RequestTimeout) so write and processing share one budget. The error
	// is intentionally ignored — ResponseWriters that do not support
	// deadlines (httptest's recorder, for one) report http.ErrNotSupported
	// here and there is no useful action to take.
	if deadline, ok := r.Context().Deadline(); ok {
		_ = http.NewResponseController(w).SetWriteDeadline(deadline)
	}

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
