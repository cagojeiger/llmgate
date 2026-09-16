package embeddings

import (
	"encoding/json"
	"net/http"

	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/telemetry"
	"llmgate/internal/platform/http/response"
)

func (h *Handler) serveEmbedding(w http.ResponseWriter, r *http.Request, req *llmtypes.EmbeddingRequest, rec *telemetry.AuditEvent, call *telemetry.CallEvent) *llmtypes.EmbeddingResponse {
	result, err := h.service.Embed(r.Context(), req)
	if result != nil {
		call.Attempts, call.Vendor, call.ModelUsed = result.Attempts, result.Vendor, result.ModelUsed
	}
	if err == nil && (result == nil || result.Response == nil) {
		err = &llmtypes.Error{Kind: llmtypes.KindEmpty, Message: "empty embedding response"}
	}
	if err != nil {
		adoptError(rec, err)
		response.WriteError(w, err)
		return nil
	}
	out, err := json.Marshal(result.Response)
	if err != nil {
		perr := &llmtypes.Error{Kind: llmtypes.KindUnknown, Message: "encode embedding response", Cause: err}
		adoptError(rec, perr)
		response.WriteError(w, perr)
		return nil
	}
	call.ResponseBytes, call.Usage = int64(len(out)), result.Response.Usage
	if deadline, ok := r.Context().Deadline(); ok {
		_ = http.NewResponseController(w).SetWriteDeadline(deadline)
	}
	w.Header().Set("Content-Type", "application/json")
	rec.StatusCode = http.StatusOK
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(out); err != nil {
		rec.Kind = llmtypes.KindClientClosed
		telemetry.SetCallKind(call, rec.Kind)
		return nil
	}
	return result.Response
}
