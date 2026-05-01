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

	"llmgate/internal/audit"
	"llmgate/internal/provider"
)

const maxChatRequestBytes = 1 << 20

type Handler struct {
	provider provider.Provider
	log      *slog.Logger
	recorder audit.Recorder
}

func NewHandler(p provider.Provider, log *slog.Logger, recorder audit.Recorder) *Handler {
	if log == nil {
		log = slog.Default()
	}
	if recorder == nil {
		recorder = audit.Nop{}
	}
	return &Handler{provider: p, log: log, recorder: recorder}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	// Install the attempts holder so router (or any provider in the chain)
	// can append per-attempt records that we then drain into the audit
	// Record below.
	ctx := provider.WithAttemptHolder(r.Context())
	r = r.WithContext(ctx)

	rec := &audit.Record{
		Timestamp: start,
		RequestID: RequestIDFromContext(ctx),
		Method:    "chat.completions",
	}
	defer func() {
		rec.DurationMS = time.Since(start).Milliseconds()
		rec.Attempts = provider.AttemptsFromContext(ctx)
		// Vendor / ModelUsed reflect the attempt that actually returned
		// the response body (the last one for success; the last failed
		// attempt otherwise). Skipped (circuit-open) entries don't
		// produce attempts so this remains accurate.
		if last := lastAttempt(rec.Attempts); last != nil {
			rec.Vendor = last.Vendor
			rec.ModelUsed = last.Model
		}
		h.recorder.Record(ctx, rec)
	}()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxChatRequestBytes))
	if err != nil {
		perr := &provider.Error{Kind: provider.KindBadRequest, Message: "read request body: " + err.Error()}
		rec.ErrorKind = perr.Kind
		rec.StatusCode = errStatus(perr)
		writeError(w, perr)
		return
	}
	rec.RequestBytes = int64(len(body))

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		perr := &provider.Error{Kind: provider.KindBadRequest, Message: "decode request JSON: " + err.Error()}
		rec.ErrorKind = perr.Kind
		rec.StatusCode = errStatus(perr)
		writeError(w, perr)
		return
	}

	req := &provider.Request{}
	if err := json.Unmarshal(body, req); err != nil {
		perr := &provider.Error{Kind: provider.KindBadRequest, Message: "decode request: " + err.Error()}
		rec.ErrorKind = perr.Kind
		rec.StatusCode = errStatus(perr)
		writeError(w, perr)
		return
	}
	rec.ModelRequested = req.Model

	streaming, _ := raw["stream"].(bool)
	if streaming {
		rec.Method = "chat.completions.stream"
		h.serveStream(w, r, req, rec)
		return
	}
	h.serveComplete(w, r, req, rec)
}

func (h *Handler) serveComplete(w http.ResponseWriter, r *http.Request, req *provider.Request, rec *audit.Record) {
	resp, err := h.provider.Complete(r.Context(), req)
	if err != nil {
		var perr *provider.Error
		if errors.As(err, &perr) {
			rec.ErrorKind = perr.Kind
		}
		rec.StatusCode = errStatus(err)
		writeError(w, err)
		return
	}

	out, err := json.Marshal(resp)
	if err != nil {
		perr := &provider.Error{Kind: provider.KindUnknown, Message: "encode response: " + err.Error(), Cause: err}
		rec.ErrorKind = perr.Kind
		rec.StatusCode = errStatus(perr)
		writeError(w, perr)
		return
	}

	rec.StatusCode = http.StatusOK
	rec.Usage = resp.Usage
	if cost, ok := resp.Extra["cost"]; ok && len(cost) > 0 {
		rec.VendorCost = string(cost)
	}
	rec.ResponseBytes = int64(len(out))

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

func (h *Handler) serveStream(w http.ResponseWriter, r *http.Request, req *provider.Request, rec *audit.Record) {
	stream, err := h.provider.CompleteStream(r.Context(), req)
	if err != nil {
		var perr *provider.Error
		if errors.As(err, &perr) {
			rec.ErrorKind = perr.Kind
		}
		rec.StatusCode = errStatus(err)
		writeError(w, err)
		return
	}
	defer stream.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		perr := &provider.Error{Kind: provider.KindUnknown, Message: "streaming unsupported"}
		rec.ErrorKind = perr.Kind
		rec.StatusCode = errStatus(perr)
		writeError(w, perr)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	rec.StatusCode = http.StatusOK

	for {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			n, _ := w.Write([]byte("data: [DONE]\n\n"))
			rec.ResponseBytes += int64(n)
			flusher.Flush()
			return
		}
		if err != nil {
			var perr *provider.Error
			if errors.As(err, &perr) {
				rec.ErrorKind = perr.Kind
			}
			h.log.LogAttrs(r.Context(), slog.LevelWarn, "stream receive failed",
				slog.String("provider", h.provider.Name()),
				slog.String("err", err.Error()),
			)
			writeSSEError(w, err)
			n, _ := w.Write([]byte("data: [DONE]\n\n"))
			rec.ResponseBytes += int64(n)
			flusher.Flush()
			return
		}

		if event.Usage != nil {
			rec.Usage = event.Usage
		}
		if cost, ok := event.Extra["cost"]; ok && len(cost) > 0 {
			rec.VendorCost = string(cost)
		}

		out, err := json.Marshal(event)
		if err != nil {
			perr := &provider.Error{Kind: provider.KindUnknown, Message: "encode stream event: " + err.Error(), Cause: err}
			rec.ErrorKind = perr.Kind
			writeSSEError(w, perr)
			n, _ := w.Write([]byte("data: [DONE]\n\n"))
			rec.ResponseBytes += int64(n)
			flusher.Flush()
			return
		}
		n, _ := fmt.Fprintf(w, "data: %s\n\n", out)
		rec.ResponseBytes += int64(n)
		flusher.Flush()
	}
}

// lastAttempt returns the last attempt in the slice (which represents
// either the successful upstream call or the final failure). Returns nil
// for an empty slice.
func lastAttempt(atts []provider.Attempt) *provider.Attempt {
	if len(atts) == 0 {
		return nil
	}
	return &atts[len(atts)-1]
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

func errStatus(err error) int {
	status, _, _ := errorPayload(err)
	return status
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
