// Package audio serves the OpenAI /v1/audio/transcriptions route. It mirrors
// the chat handler's shape — parse, route, write, then emit the operational
// audit event, the call event, and the full-body result event — so a
// transcription request is recorded exactly like a chat completion.
package audio

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
)

// TranscribeService is the upstream contract Handler needs.
type TranscribeService interface {
	Transcribe(ctx context.Context, req *llmtypes.TranscriptionRequest) (*routing.TranscribeResult, error)
	TranscribeStream(ctx context.Context, req *llmtypes.TranscriptionRequest) (*routing.TranscribeResult, error)
}

type Handler struct {
	service         TranscribeService
	log             *slog.Logger
	events          telemetry.EventSink
	results         llmresultsink.Sink
	lifecycle       telemetry.LifecycleObserver
	serviceVersion  string
	environment     string
	requestTimeout  time.Duration
	maxRequestBytes int64
}

type HandlerConfig struct {
	RequestTimeout time.Duration
	// MaxRequestBytes caps the request body; <= 0 falls back to the default.
	MaxRequestBytes   int64
	ServiceVersion    string
	Environment       string
	LifecycleObserver telemetry.LifecycleObserver
	ResultSink        llmresultsink.Sink
}

func NewHandler(service TranscribeService, log *slog.Logger, events telemetry.EventSink, cfg HandlerConfig) *Handler {
	if log == nil {
		log = slog.Default()
	}
	if events == nil {
		events = telemetry.NopSink{}
	}
	events = telemetry.NewRecoveringSink(events, log)
	// A nil or no-op ResultSink leaves h.results nil so the finish defer skips
	// building result events entirely — FromTranscription deep-clones the full
	// audio payload, which is real work to construct just to drop.
	var results llmresultsink.Sink
	if cfg.ResultSink != nil {
		if _, nop := cfg.ResultSink.(llmresultsink.NopSink); !nop {
			results = llmresultsink.NewRecoveringSink(cfg.ResultSink, log)
		}
	}
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
	maxRequestBytes := cfg.MaxRequestBytes
	if maxRequestBytes <= 0 {
		maxRequestBytes = defaultMaxAudioRequestBytes
	}
	return &Handler{
		service:         service,
		log:             log,
		events:          events,
		results:         results,
		lifecycle:       lifecycle,
		serviceVersion:  serviceVersion,
		environment:     environment,
		requestTimeout:  cfg.RequestTimeout,
		maxRequestBytes: maxRequestBytes,
	}
}

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
		Operation:      "audio.transcriptions",
		ConsumerName:   consumer.Name,
		ConsumerKeyID:  consumer.KeyID,
	})
	rec := telemetry.NewAuditEvent(common)
	telemetry.MarkAuthSuccess(rec)
	var call *telemetry.CallEvent
	var req *llmtypes.TranscriptionRequest
	var resultResp *llmtypes.TranscriptionResponse
	defer func() {
		telemetry.FinishAuditEvent(rec, rec.StatusCode, rec.Kind, time.Since(start).Milliseconds())
		h.events.Emit(ctx, rec)
		if telemetry.CallAttempted(call) {
			telemetry.FinishCallFromAudit(call, rec)
			h.events.Emit(ctx, call)
		}
		if h.results != nil {
			if ev, ok := llmresultschema.FromTranscription(llmresultschema.TranscriptionBuildInput{
				Audit:    rec,
				Call:     call,
				Request:  req,
				Response: resultResp,
			}); ok {
				h.results.Emit(ctx, ev)
			}
		}
	}()
	// Registered after the audit defer so it runs first and stamps the record
	// before the audit-always hook observes it.
	defer func() {
		if p := recover(); p != nil {
			h.recoverPanic(ctx, w, rec, p)
		}
	}()

	if consumer.AuthError != "" {
		telemetry.MarkAuthFailure(rec, consumer.AuthError)
		perr := &llmtypes.Error{Kind: llmtypes.KindAuth, Message: "unauthorized"}
		adoptError(rec, perr)
		response.WriteError(w, perr)
		return
	}

	req, requestBytes, err := decodeTranscriptionRequest(w, r, h.maxRequestBytes)
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
		rec.Operation = "audio.transcriptions.stream"
		call.Operation = "audio.transcriptions.stream"
		resultResp = h.serveTranscriptionStream(w, r, req, rec, call)
		return
	}
	resultResp = h.serveTranscription(w, r, req, rec, call)
}

// recoverPanic stamps panic outcomes for audit and preserves
// http.ErrAbortHandler's abort semantics.
func (h *Handler) recoverPanic(ctx context.Context, w http.ResponseWriter, rec *telemetry.AuditEvent, p any) {
	if err, ok := p.(error); ok && errors.Is(err, http.ErrAbortHandler) {
		panic(p)
	}
	rec.Kind = llmtypes.KindPanic
	rec.StatusCode = http.StatusInternalServerError
	h.log.LogAttrs(ctx, slog.LevelError, "handler panic",
		slog.String("request_id", rec.RequestID),
		slog.Any("panic", p),
		slog.String("stack", string(debug.Stack())),
	)
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
