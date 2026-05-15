package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"llmgate/internal/audit"
	"llmgate/internal/llmrouter"
	"llmgate/internal/llmtypes"
)

const maxChatRequestBytes = 1 << 20

// ChatService is the upstream contract Handler needs.
type ChatService interface {
	Complete(ctx context.Context, req *llmtypes.Request) (*llmrouter.RouteResult, error)
	CompleteStream(ctx context.Context, req *llmtypes.Request) (*llmrouter.RouteResult, error)
}

type Handler struct {
	service        ChatService
	log            *slog.Logger
	recorder       audit.Recorder
	callRecorder   audit.CallRecorder
	requestTimeout time.Duration
	stream         *streamRelay
}

type HandlerConfig struct {
	RequestTimeout    time.Duration
	StreamIdleTimeout time.Duration
}

func NewHandler(service ChatService, log *slog.Logger, recorder audit.Recorder, callRecorder audit.CallRecorder, cfg HandlerConfig) *Handler {
	if log == nil {
		log = slog.Default()
	}
	if recorder == nil {
		recorder = audit.Nop{}
	}
	if callRecorder == nil {
		callRecorder = nopCallRecorder{}
	}
	return &Handler{
		service:        service,
		log:            log,
		recorder:       recorder,
		callRecorder:   callRecorder,
		requestTimeout: cfg.RequestTimeout,
		stream:         newStreamRelay(log, cfg.StreamIdleTimeout),
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ctx := r.Context()
	if h.requestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, h.requestTimeout)
		defer cancel()
		r = r.WithContext(ctx)
	}

	consumer := ConsumerFromContext(ctx)
	common := audit.EventCommon{
		Timestamp:     start,
		RequestID:     RequestIDFromContext(ctx),
		Operation:     "chat.completions",
		ConsumerName:  consumer.Name,
		ConsumerKeyID: consumer.KeyID,
	}
	rec := &audit.Record{EventCommon: common}
	var call *audit.CallRecord
	defer func() {
		rec.DurationMS = time.Since(start).Milliseconds()
		h.recorder.RecordAudit(ctx, rec)
		if call != nil && len(call.Attempts) > 0 {
			call.DurationMS = rec.DurationMS
			call.StatusCode = rec.StatusCode
			call.Kind = rec.Kind
			h.callRecorder.RecordCall(ctx, call)
		}
	}()
	// Registered after the audit defer so it runs first and stamps the
	// record before the audit-always hook observes it.
	defer func() {
		if p := recover(); p != nil {
			h.recoverPanic(ctx, w, rec, p)
		}
	}()

	if consumer.AuthError != "" {
		// Auth middleware ran but rejected; emit the audit record
		// (audit-always — ADR 003) and return 401. The specific
		// audit.AuthError stays out of the wire response — callers see
		// only "unauthorized" — but is stamped on rec.AuthError so
		// audit/access-log surfaces show "missing" vs "format" vs
		// "unknown" for operators.
		rec.AuthError = consumer.AuthError
		perr := &llmtypes.Error{Kind: llmtypes.KindAuth, Message: "unauthorized"}
		adoptError(rec, perr)
		writeError(w, perr)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxChatRequestBytes))
	if err != nil {
		perr := &llmtypes.Error{Kind: llmtypes.KindBadRequest, Message: "read request body: " + err.Error()}
		adoptError(rec, perr)
		writeError(w, perr)
		return
	}
	requestBytes := int64(len(body))

	req := &llmtypes.Request{}
	if err := json.Unmarshal(body, req); err != nil {
		perr := &llmtypes.Error{Kind: llmtypes.KindBadRequest, Message: "decode request: " + err.Error()}
		adoptError(rec, perr)
		writeError(w, perr)
		return
	}
	if req.Model != "" && !isModelAllowed(req.Model, consumer.AllowedAliases) {
		perr := &llmtypes.Error{Kind: llmtypes.KindForbidden, Message: "model not allowed"}
		adoptError(rec, perr)
		writeError(w, perr)
		return
	}

	call = &audit.CallRecord{
		EventCommon:    common,
		ModelRequested: req.Model,
		RequestBytes:   requestBytes,
	}
	if req.Stream != nil && *req.Stream {
		rec.Operation = "chat.completions.stream"
		call.Operation = "chat.completions.stream"
		h.serveStream(w, r, req, rec, call)
		return
	}
	h.serveComplete(w, r, req, rec, call)
}

func isModelAllowed(model string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, alias := range allowed {
		if strings.EqualFold(model, alias) {
			return true
		}
	}
	return false
}

// recoverPanic stamps panic outcomes for audit, preserves
// http.ErrAbortHandler's abort semantics, and avoids writing a JSON
// envelope after a streaming response has already started.
func (h *Handler) recoverPanic(ctx context.Context, w http.ResponseWriter, rec *audit.Record, p any) {
	if p == http.ErrAbortHandler {
		panic(p)
	}
	rec.Kind = llmtypes.KindPanic
	// Audit status records the outcome, so a panic overrides any prior
	// status stamp such as the 200 set when SSE headers flush.
	rec.StatusCode = http.StatusInternalServerError
	h.log.LogAttrs(ctx, slog.LevelError, "handler panic",
		slog.String("request_id", rec.RequestID),
		slog.Any("panic", p),
		slog.String("stack", string(debug.Stack())),
	)
	// A second WriteHeader is ignored, but body bytes would still corrupt
	// an in-flight SSE stream.
	if cw, ok := w.(*countingWriter); ok && cw.wroteHeader {
		return
	}
	writeError(w, &llmtypes.Error{Kind: llmtypes.KindUnknown, Message: "internal server error"})
}

// adoptRouteCall copies routing metadata onto call.
func adoptRouteCall(call *audit.CallRecord, result *llmrouter.RouteResult) {
	if result == nil {
		return
	}
	call.Attempts = result.Attempts
	call.Vendor = result.Vendor
	call.ModelUsed = result.ModelUsed
}

// adoptError populates rec.Kind and rec.StatusCode from err.
func adoptError(rec *audit.Record, err error) {
	rec.Kind = llmtypes.ErrorKindOf(err)
	rec.StatusCode = errStatus(err)
}

// adoptStreamSummaryCall finalizes stream call fields after the stream ends.
func adoptStreamSummaryCall(call *audit.CallRecord, sum *llmtypes.Summary, now time.Time) {
	if len(call.Attempts) > 0 {
		last := &call.Attempts[len(call.Attempts)-1]
		last.DurationMS = now.Sub(last.StartedAt).Milliseconds()
		if last.Kind == "" && call.Kind != "" {
			last.Kind = call.Kind
		}
		if sum != nil {
			if sum.Usage != nil {
				last.Usage = sum.Usage
			}
			if sum.VendorCost != "" {
				last.VendorCost = sum.VendorCost
			}
		}
	}
	if sum == nil {
		return
	}
	if sum.Usage != nil {
		call.Usage = sum.Usage
	}
	if sum.VendorCost != "" {
		call.VendorCost = sum.VendorCost
	}
}

func (h *Handler) serveComplete(w http.ResponseWriter, r *http.Request, req *llmtypes.Request, rec *audit.Record, call *audit.CallRecord) {
	result, err := h.service.Complete(r.Context(), req)
	adoptRouteCall(call, result)
	if err != nil {
		adoptError(rec, err)
		writeError(w, err)
		return
	}

	out, err := json.Marshal(result.Response)
	if err != nil {
		perr := &llmtypes.Error{Kind: llmtypes.KindUnknown, Message: "encode response: " + err.Error(), Cause: err}
		adoptError(rec, perr)
		writeError(w, perr)
		return
	}

	rec.StatusCode = http.StatusOK
	call.ResponseBytes = int64(len(out))
	if result.Response != nil {
		call.Usage = result.Response.Usage
		if cost, ok := result.Response.Extra["cost"]; ok && len(cost) > 0 {
			call.VendorCost = string(cost)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, werr := w.Write(out); werr != nil {
		rec.Kind = llmtypes.KindClientClosed
		call.Kind = rec.Kind
		h.log.LogAttrs(r.Context(), slog.LevelInfo, "client write failed",
			slog.String("vendor", call.Vendor),
			slog.String("err", werr.Error()),
		)
	}
}

func (h *Handler) serveStream(w http.ResponseWriter, r *http.Request, req *llmtypes.Request, rec *audit.Record, call *audit.CallRecord) {
	result, err := h.service.CompleteStream(r.Context(), req)
	adoptRouteCall(call, result)
	if err != nil {
		adoptError(rec, err)
		writeError(w, err)
		return
	}
	stream := result.Stream
	defer stream.Close()
	defer func() { adoptStreamSummaryCall(call, stream.Summary(), time.Now()) }()

	h.stream.Run(r.Context(), w, stream, rec, call)
}

type nopCallRecorder struct{}

func (nopCallRecorder) RecordCall(context.Context, *audit.CallRecord) {}
func (nopCallRecorder) Close() error                                  { return nil }
