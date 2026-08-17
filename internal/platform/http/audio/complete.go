package audio

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/routing"
	"llmgate/internal/domain/telemetry"
	"llmgate/internal/platform/http/response"
)

func (h *Handler) serveTranscription(w http.ResponseWriter, r *http.Request, req *llmtypes.TranscriptionRequest, rec *telemetry.AuditEvent, call *telemetry.CallEvent) *llmtypes.TranscriptionResponse {
	result, err := h.service.Transcribe(r.Context(), req)
	adoptRouteResult(call, result)
	if err != nil {
		adoptError(rec, err)
		response.WriteError(w, err)
		return nil
	}

	out, contentType, err := encodeTranscription(req.ResponseFormat, result.Response)
	if err != nil {
		perr := &llmtypes.Error{Kind: llmtypes.KindUnknown, Message: "encode response: " + err.Error(), Cause: err}
		adoptError(rec, perr)
		response.WriteError(w, perr)
		return nil
	}

	rec.StatusCode = http.StatusOK
	call.ResponseBytes = int64(len(out))

	// Mirror chat: bound the write to the request deadline so a slow reader
	// cannot outlive the request budget. Recorders without deadline support
	// return http.ErrNotSupported, which has no useful action.
	if deadline, ok := r.Context().Deadline(); ok {
		_ = http.NewResponseController(w).SetWriteDeadline(deadline)
	}

	w.Header().Set("Content-Type", contentType)
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

// encodeTranscription renders the response in the caller's requested format:
// text / srt / vtt emit the raw transcript as text/plain; json (default) and
// verbose_json emit the JSON object. omitempty on the timing fields means the
// plain json format collapses to {"text":...} while verbose_json keeps them.
func encodeTranscription(responseFormat string, resp *llmtypes.TranscriptionResponse) ([]byte, string, error) {
	if resp == nil {
		resp = &llmtypes.TranscriptionResponse{}
	}
	switch strings.ToLower(responseFormat) {
	case "text", "srt", "vtt":
		return []byte(resp.Text), "text/plain; charset=utf-8", nil
	default: // "", json, verbose_json
		out, err := json.Marshal(resp)
		if err != nil {
			return nil, "", err
		}
		return out, "application/json", nil
	}
}

// adoptRouteResult copies the routing outcome onto the call event. The
// TranscribeResult mirrors chat's RouteResult fields, so this sets them
// directly rather than through a chat-typed telemetry helper.
func adoptRouteResult(c *telemetry.CallEvent, result *routing.TranscribeResult) {
	if c == nil || result == nil {
		return
	}
	c.Attempts = result.Attempts
	c.Vendor = result.Vendor
	c.ModelUsed = result.ModelUsed
}
