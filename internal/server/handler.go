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

	"llmgate/internal/domain/llmresult"
	llmresultsink "llmgate/internal/domain/llmresult/sink"
	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/routing"
	"llmgate/internal/platform/http/response"
	"llmgate/internal/domain/telemetry"
)

const maxChatRequestBytes = 1 << 20

// ChatService is the upstream contract Handler needs.
type ChatService interface {
	Complete(ctx context.Context, req *llmtypes.Request) (*routing.RouteResult, error)
	CompleteStream(ctx context.Context, req *llmtypes.Request) (*routing.RouteResult, error)
}

type Handler struct {
	service        ChatService
	log            *slog.Logger
	events         telemetry.EventSink
	results        llmresultsink.Sink
	lifecycle      telemetry.LifecycleObserver
	serviceVersion string
	environment    string
	requestTimeout time.Duration
	stream         *streamRelay
}

type HandlerConfig struct {
	RequestTimeout    time.Duration
	StreamIdleTimeout time.Duration
	ServiceVersion    string
	Environment       string
	LifecycleObserver telemetry.LifecycleObserver
	ResultSink        llmresultsink.Sink
}

func NewHandler(service ChatService, log *slog.Logger, events telemetry.EventSink, cfg HandlerConfig) *Handler {
	if log == nil {
		log = slog.Default()
	}
	if events == nil {
		events = telemetry.NopSink{}
	}
	events = telemetry.NewRecoveringSink(events, log)
	results := llmresultsink.NewRecoveringSink(cfg.ResultSink, log)
	lifecycle := cfg.LifecycleObserver
	if lifecycle == nil {
		lifecycle = telemetry.NopLifecycleObserver{}
	}
	lifecycle = telemetry.NewLifecycleObservers(log, lifecycle)
	serviceVersion := cfg.ServiceVersion
	if serviceVersion == "" {
		serviceVersion = "dev"
	}
	environment := cfg.Environment
	if environment == "" {
		environment = "local"
	}
	return &Handler{
		service:        service,
		log:            log,
		events:         events,
		results:        results,
		lifecycle:      lifecycle,
		serviceVersion: serviceVersion,
		environment:    environment,
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
	h.lifecycle.RequestStarted(ctx)
	defer h.lifecycle.RequestFinished(ctx)

	consumer := ConsumerFromContext(ctx)
	common := telemetry.NewEventCommon(telemetry.CommonInput{
		Timestamp:      start,
		RequestID:      RequestIDFromContext(ctx),
		ServiceVersion: h.serviceVersion,
		Environment:    h.environment,
		Operation:      "chat.completions",
		ConsumerName:   consumer.Name,
		ConsumerKeyID:  consumer.KeyID,
	})
	rec := telemetry.NewAuditEvent(common)
	telemetry.MarkAuthSuccess(rec)
	var call *telemetry.CallEvent
	var req *llmtypes.Request
	results := newResultRecorder(h.results)
	defer func() {
		telemetry.FinishAuditEvent(rec, rec.StatusCode, rec.Kind, time.Since(start).Milliseconds())
		h.events.Emit(ctx, rec)
		if telemetry.CallAttempted(call) {
			telemetry.FinishCallFromAudit(call, rec)
			h.events.Emit(ctx, call)
		}
		results.Emit(ctx, rec, call)
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
		// AuthError stays out of the wire response — callers see
		// only "unauthorized" — but is stamped on rec.AuthError so
		// audit/access-log surfaces show "missing" vs "format" vs
		// "unknown" for operators.
		telemetry.MarkAuthFailure(rec, consumer.AuthError)
		perr := &llmtypes.Error{Kind: llmtypes.KindAuth, Message: "unauthorized"}
		adoptError(rec, perr)
		response.WriteError(w, perr)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxChatRequestBytes))
	if err != nil {
		perr := &llmtypes.Error{Kind: llmtypes.KindBadRequest, Message: "read request body: " + err.Error()}
		adoptError(rec, perr)
		response.WriteError(w, perr)
		return
	}
	requestBytes := int64(len(body))

	req = &llmtypes.Request{}
	if err := json.Unmarshal(body, req); err != nil {
		perr := &llmtypes.Error{Kind: llmtypes.KindBadRequest, Message: "decode request: " + err.Error()}
		adoptError(rec, perr)
		response.WriteError(w, perr)
		return
	}
	results.Request(req)
	telemetry.SetResource(rec, "llm_model", req.Model)
	if req.Model != "" && !isModelAllowed(req.Model, consumer.AllowedAliases) {
		telemetry.MarkPolicyDenied(rec, telemetry.DenyReasonModelNotAllowed)
		perr := &llmtypes.Error{Kind: llmtypes.KindForbidden, Message: "model not allowed"}
		adoptError(rec, perr)
		response.WriteError(w, perr)
		return
	}
	telemetry.MarkPolicyAllowed(rec)

	call = telemetry.NewCallEvent(common, req.Model, requestBytes)
	if req.Stream != nil && *req.Stream {
		rec.Operation = "chat.completions.stream"
		call.Operation = "chat.completions.stream"
		results.Response(h.serveStream(w, r, req, rec, call))
		return
	}
	results.Response(h.serveComplete(w, r, req, rec, call))
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
func (h *Handler) recoverPanic(ctx context.Context, w http.ResponseWriter, rec *telemetry.AuditEvent, p any) {
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
	if started, ok := w.(interface{ WroteHeader() bool }); ok && started.WroteHeader() {
		return
	}
	response.WriteError(w, &llmtypes.Error{Kind: llmtypes.KindUnknown, Message: "internal server error"})
}

// adoptError populates rec.Kind and rec.StatusCode from err.
func adoptError(rec *telemetry.AuditEvent, err error) {
	rec.Kind = llmtypes.ErrorKindOf(err)
	rec.StatusCode = response.Status(err)
}

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

	builder := llmresult.NewStreamResponseBuilder()
	h.stream.Run(r.Context(), w, stream, rec, call, builder.Add)
	if rec.Kind != "" {
		return nil
	}
	return builder.Response()
}
