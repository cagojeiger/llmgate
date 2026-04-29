package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"llmgate/internal/provider"
)

const maxChatRequestBytes = 1 << 20

type Handler struct {
	provider provider.Provider
	log      *slog.Logger
}

func NewHandler(p provider.Provider, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{provider: p, log: log}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxChatRequestBytes))
	if err != nil {
		writeError(w, &provider.Error{Kind: provider.KindBadRequest, Message: "read request body: " + err.Error()})
		return
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		writeError(w, &provider.Error{Kind: provider.KindBadRequest, Message: "decode request JSON: " + err.Error()})
		return
	}

	req := &provider.Request{}
	if err := json.Unmarshal(body, req); err != nil {
		writeError(w, &provider.Error{Kind: provider.KindBadRequest, Message: "decode request: " + err.Error()})
		return
	}

	streaming, _ := raw["stream"].(bool)
	if streaming {
		h.serveStream(w, r, req)
		return
	}
	h.serveComplete(w, r, req)
}

func (h *Handler) serveComplete(w http.ResponseWriter, r *http.Request, req *provider.Request) {
	resp, err := h.provider.Complete(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}

	out, err := json.Marshal(resp)
	if err != nil {
		writeError(w, &provider.Error{Kind: provider.KindUnknown, Message: "encode response: " + err.Error(), Cause: err})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

func (h *Handler) serveStream(w http.ResponseWriter, r *http.Request, req *provider.Request) {
	stream, err := h.provider.CompleteStream(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	defer stream.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, &provider.Error{Kind: provider.KindUnknown, Message: "streaming unsupported"})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			flusher.Flush()
			return
		}
		if err != nil {
			h.log.LogAttrs(r.Context(), slog.LevelWarn, "stream receive failed",
				slog.String("provider", h.provider.Name()),
				slog.String("err", err.Error()),
			)
			writeSSEError(w, err)
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			flusher.Flush()
			return
		}

		out, err := json.Marshal(event)
		if err != nil {
			writeSSEError(w, &provider.Error{Kind: provider.KindUnknown, Message: "encode stream event: " + err.Error(), Cause: err})
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			flusher.Flush()
			return
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", out)
		flusher.Flush()
	}
}

func writeError(w http.ResponseWriter, err error) {
	status, retryAfter, payload := errorPayload(err)
	w.Header().Set("Content-Type", "application/json")
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.FormatInt(int64(math.Ceil(retryAfter.Seconds())), 10))
	}
	w.WriteHeader(status)
	_, _ = w.Write(append(payload, '\n'))
}

func writeSSEError(w http.ResponseWriter, err error) {
	_, _, payload := errorPayload(err)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
}

func errorPayload(err error) (int, time.Duration, []byte) {
	status, kind, message, code, retryAfter := errorDetails(err)
	payload := struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
			Status  int    `json:"status"`
		} `json:"error"`
	}{}
	payload.Error.Message = message
	payload.Error.Type = string(kind)
	payload.Error.Code = code
	payload.Error.Status = status

	out, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		return http.StatusInternalServerError, 0, []byte(`{"error":{"message":"encode error","type":"unknown","code":null,"status":500}}`)
	}
	return status, retryAfter, out
}

func errorDetails(err error) (int, provider.Kind, string, any, time.Duration) {
	var perr *provider.Error
	if !errors.As(err, &perr) {
		if err == nil {
			return http.StatusInternalServerError, provider.KindUnknown, "unknown error", nil, 0
		}
		return http.StatusInternalServerError, provider.KindUnknown, err.Error(), nil, 0
	}

	status := http.StatusInternalServerError
	code := any(nil)
	switch perr.Kind {
	case provider.KindAuth:
		status = http.StatusUnauthorized
	case provider.KindRateLimit:
		status = http.StatusTooManyRequests
	case provider.KindBadRequest:
		status = http.StatusBadRequest
	case provider.KindContextLength:
		status = http.StatusBadRequest
		code = "context_length_exceeded"
	case provider.KindContentFilter:
		status = http.StatusBadRequest
		code = "content_filter"
	case provider.KindUpstream, provider.KindNetwork, provider.KindTimeout, provider.KindEmpty:
		status = http.StatusBadGateway
	case provider.KindUnknown:
		status = http.StatusInternalServerError
	}

	kind := perr.Kind
	if kind == "" {
		kind = provider.KindUnknown
	}
	message := perr.Message
	if message == "" {
		message = http.StatusText(status)
	}
	return status, kind, message, code, perr.RetryAfter
}
