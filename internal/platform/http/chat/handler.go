package chat

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	llmresultschema "llmgate/internal/domain/llmresult/schema"
	llmresultsink "llmgate/internal/domain/llmresult/sink"
	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/routing"
	"llmgate/internal/domain/telemetry"
	httpauth "llmgate/internal/platform/http/auth"
	"llmgate/internal/platform/http/requestid"
	"llmgate/internal/platform/http/response"
	httpstream "llmgate/internal/platform/http/stream"
)

// ChatService is the upstream contract Handler needs.
type ChatService interface {
	Complete(ctx context.Context, req *llmtypes.Request) (*routing.RouteResult, error)
	CompleteStream(ctx context.Context, req *llmtypes.Request) (*routing.RouteResult, error)
}

// Handler is the HTTP entry point for OpenAI-wire chat.completions.
// It owns per-request lifecycle (timeout context, lifecycle observer
// hooks, panic recovery), branches stream vs non-stream off the
// request body's "stream" flag, and stamps the audit / call /
// llm-result telemetry records that get emitted after the response.
type Handler struct {
	service        ChatService
	log            *slog.Logger
	events         telemetry.EventSink
	results        llmresultsink.Sink
	lifecycle      telemetry.LifecycleObserver
	serviceVersion string
	environment    string
	requestTimeout time.Duration
	stream         *httpstream.Relay
}

// HandlerConfig is the operator-tunable surface for NewHandler.
//
//   - RequestTimeout caps the full request including streaming
//     response — must be positive (zero is rejected by the env
//     loader to stop the timeout=0=disabled anti-pattern).
//   - StreamIdleTimeout caps the gap between SSE events from
//     upstream; expires sooner than RequestTimeout for hung
//     vendors.
//   - ResultSink receives one llmresult.Event per finalized request;
//     LifecycleObserver receives RequestStarted / RequestFinished /
//     StreamFinished hooks (typically the Prometheus recorder).
//   - ServiceVersion / Environment are stamped on every audit and
//     call event for cross-fleet correlation.
type HandlerConfig struct {
	RequestTimeout    time.Duration
	StreamIdleTimeout time.Duration
	ServiceVersion    string
	Environment       string
	LifecycleObserver telemetry.LifecycleObserver
	ResultSink        llmresultsink.Sink
}

// NewHandler wires a Handler with safe defaults: nil log →
// slog.Default, nil events / lifecycle observer → no-op stubs,
// missing ServiceVersion / Environment → "dev" / "local". Both the
// audit event sink and the llm-result sink are wrapped in a
// panic-recovering layer so a downstream broker hang or schema bug
// can never escape into the request goroutine and crash the
// process.
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
		stream:         httpstream.NewRelay(log, cfg.StreamIdleTimeout),
	}
}

// ServeHTTP handles one OpenAI-wire chat request. The request body's
// "stream" bit selects the wire format: stream=true goes through the
// SSE Relay; non-stream goes through serveComplete and renders one
// JSON response. Both paths stamp the same audit / call records,
// which are emitted by the deferred lifecycle hooks after the
// response is on the wire (see inline comments for the defer
// ordering invariants).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), h.requestTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	h.lifecycle.RequestStarted(ctx)
	defer h.lifecycle.RequestFinished(ctx)

	consumer := httpauth.FromContext(ctx)
	common := telemetry.NewEventCommon(telemetry.CommonInput{
		Timestamp:      start,
		RequestID:      requestid.FromContext(ctx),
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
	var resultResp *llmtypes.Response
	defer func() {
		telemetry.FinishAuditEvent(rec, rec.StatusCode, rec.Kind, time.Since(start).Milliseconds())
		h.events.Emit(ctx, rec)
		if telemetry.CallAttempted(call) {
			telemetry.FinishCallFromAudit(call, rec)
			h.events.Emit(ctx, call)
		}
		if h.results != nil {
			if ev, ok := llmresultschema.FromTelemetry(llmresultschema.BuildInput{
				Audit:    rec,
				Call:     call,
				Request:  req,
				Response: resultResp,
			}); ok {
				h.results.Emit(ctx, ev)
			}
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

	req, requestBytes, err := decodeChatRequest(w, r)
	if err != nil {
		adoptError(rec, err)
		response.WriteError(w, err)
		return
	}
	if verr := req.Validate(); verr != nil {
		adoptError(rec, verr)
		response.WriteError(w, verr)
		return
	}
	telemetry.SetResource(rec, "llm_model", req.Model)
	if !modelAllowed(req.Model, consumer.AllowedAliases) {
		telemetry.MarkPolicyDenied(rec, telemetry.DenyReasonModelNotAllowed)
		perr := modelNotAllowedError()
		adoptError(rec, perr)
		response.WriteError(w, perr)
		return
	}
	telemetry.MarkPolicyAllowed(rec)

	call = telemetry.NewCallEvent(common, req.Model, requestBytes)
	if req.Stream != nil && *req.Stream {
		rec.Operation = "chat.completions.stream"
		call.Operation = "chat.completions.stream"
		resultResp = h.serveStream(w, r, req, rec, call)
		return
	}
	resultResp = h.serveComplete(w, r, req, rec, call)
}

// recoverPanic stamps panic outcomes for audit, preserves
// http.ErrAbortHandler's abort semantics, and avoids writing a JSON
// envelope after a streaming response has already started.
func (h *Handler) recoverPanic(ctx context.Context, w http.ResponseWriter, rec *telemetry.AuditEvent, p any) {
	if err, ok := p.(error); ok && errors.Is(err, http.ErrAbortHandler) {
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
	// an in-flight SSE stream. HeadersWritten walks the Unwrap chain so a
	// middleware wrap between AccessLog and the handler cannot silently
	// hide the CountingWriter's signal.
	if response.HeadersWritten(w) {
		return
	}
	response.WriteError(w, &llmtypes.Error{Kind: llmtypes.KindUnknown, Message: "internal server error"})
}

// adoptError populates rec.Kind and rec.StatusCode from err.
func adoptError(rec *telemetry.AuditEvent, err error) {
	rec.Kind = llmtypes.ErrorKindOf(err)
	rec.StatusCode = response.Status(err)
}
