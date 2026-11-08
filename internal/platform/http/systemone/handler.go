// Package systemone serves the TypeSafe System One decision endpoint.
package systemone

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	llmresultschema "llmgate/internal/domain/llmresult/schema"
	llmresultsink "llmgate/internal/domain/llmresult/sink"
	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/telemetry"
	httpauth "llmgate/internal/platform/http/auth"
	"llmgate/internal/platform/http/requestid"
	"llmgate/internal/platform/upstream"
)

const (
	Model           = "jev-1.14.0"
	vendor          = "typesafe"
	defaultEndpoint = "https://api.typesafe.ai/v1/systemone"

	defaultRequestBytes = 10 << 20
	guardedRequestBytes = 1 << 20
)

type HandlerConfig struct {
	APIKey              string
	Endpoint            string
	ServiceVersion      string
	Environment         string
	RequestTimeout      time.Duration
	CompleteTimeout     time.Duration
	MaxRequestBodyBytes int64
	ResultSink          llmresultsink.Sink
	RecordResult        func(context.Context, *llmresultschema.Event) error
}

type Handler struct {
	cfg          HandlerConfig
	log          *slog.Logger
	client       *http.Client
	events       telemetry.EventSink
	results      llmresultsink.Sink
	payloadSlots chan struct{}
}

func NewHandler(log *slog.Logger, events telemetry.EventSink, cfg HandlerConfig) *Handler {
	if log == nil {
		log = slog.Default()
	}
	if events == nil {
		events = telemetry.NopSink{}
	}
	var results llmresultsink.Sink
	if cfg.ResultSink != nil {
		if _, nop := cfg.ResultSink.(llmresultsink.NopSink); !nop {
			results = llmresultsink.NewRecoveringSink(cfg.ResultSink, log)
		}
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = defaultEndpoint
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 5 * time.Minute
	}
	if cfg.CompleteTimeout <= 0 {
		cfg.CompleteTimeout = time.Minute
	}
	if cfg.MaxRequestBodyBytes <= 0 {
		cfg.MaxRequestBodyBytes = defaultRequestBytes
	}
	cfg.MaxRequestBodyBytes = min(cfg.MaxRequestBodyBytes, maxTypeSafeRequestBytes)
	client := upstream.DefaultClient()
	// Even a same-host redirect can re-submit a billable POST.
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &Handler{
		cfg: cfg, log: log, client: client, events: telemetry.NewRecoveringSink(events, log), results: results,
		// Decoding and recording one image can require several copies of
		// its payload. Limit that work independently of ordinary requests.
		payloadSlots: make(chan struct{}, 1),
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.RequestTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	consumer := httpauth.FromContext(ctx)
	common := telemetry.NewEventCommon(telemetry.CommonInput{
		Timestamp:      start,
		RequestID:      requestid.FromContext(ctx),
		ServiceVersion: h.cfg.ServiceVersion,
		Environment:    h.cfg.Environment,
		Operation:      "decision.systemone",
		ConsumerName:   consumer.Name,
		ConsumerKeyID:  consumer.KeyID,
	})
	rec := telemetry.NewAuditEvent(common)
	// The server's recovery middleware handles panics after this defer runs.
	rec.StatusCode, rec.Kind = http.StatusInternalServerError, llmtypes.KindPanic
	var call *telemetry.CallEvent
	var decoded decisionRequest
	var result json.RawMessage
	var admitted, recordAttempted bool
	defer func() {
		if admitted {
			defer func() { <-h.payloadSlots }()
		}
		telemetry.FinishAuditEvent(rec, rec.StatusCode, rec.Kind, time.Since(start).Milliseconds())
		h.events.Emit(ctx, rec)
		if !telemetry.CallAttempted(call) {
			return
		}
		telemetry.FinishCallFromAudit(call, rec)
		h.events.Emit(ctx, call)
		if !recordAttempted {
			_ = h.recordResult(ctx, rec, call, decoded, result, admitted)
		}
	}()

	if consumer.AuthError != "" || consumer.Name == "" {
		authError := consumer.AuthError
		if authError == "" {
			authError = telemetry.AuthErrorMissing
		}
		telemetry.MarkAuthFailure(rec, authError)
		fail(w, rec, http.StatusUnauthorized, llmtypes.KindAuth, "unauthorized")
		return
	}
	telemetry.MarkAuthSuccess(rec)
	telemetry.SetResource(rec, "llm_model", Model)
	if len(consumer.AllowedAliases) > 0 && !slices.ContainsFunc(consumer.AllowedAliases, func(alias string) bool {
		return strings.EqualFold(alias, Model)
	}) {
		telemetry.MarkPolicyDenied(rec, telemetry.DenyReasonModelNotAllowed)
		fail(w, rec, http.StatusForbidden, llmtypes.KindForbidden, "model not allowed")
		return
	}
	telemetry.MarkPolicyAllowed(rec)
	if h.cfg.APIKey == "" {
		fail(w, rec, http.StatusServiceUnavailable, llmtypes.KindUpstream, "TypeSafe is unavailable")
		return
	}
	if needsPayloadSlot(r) {
		select {
		case h.payloadSlots <- struct{}{}:
			admitted = true
		default:
			w.Header().Set("Retry-After", "1")
			fail(w, rec, http.StatusTooManyRequests, llmtypes.KindRateLimit, "TypeSafe request capacity is busy")
			return
		}
	}
	var problem *requestProblem
	decoded, problem = decodeRequest(w, r, h.cfg.MaxRequestBodyBytes)
	// A socket read timeout can also cause net/http to cancel the request context.
	switch requestErr := ctx.Err(); {
	case problem != nil && problem.kind == llmtypes.KindTimeout, requestErr == context.DeadlineExceeded:
		fail(w, rec, http.StatusBadGateway, llmtypes.KindTimeout, "request timed out")
		return
	case requestErr == context.Canceled:
		fail(w, rec, 499, llmtypes.KindClientClosed, "client closed the request")
		return
	case problem != nil:
		fail(w, rec, problem.status, problem.kind, problem.message)
		return
	}
	call = telemetry.NewCallEvent(common, Model, decoded.inputBytes)
	attemptCtx, cancelAttempt := context.WithTimeout(ctx, h.cfg.CompleteTimeout)
	// Keep the completed decision even if the caller disconnects during delivery.
	result = h.evaluate(w, attemptCtx, rec, call, decoded)
	cancelAttempt()
	if result == nil {
		return
	}
	rec.StatusCode, rec.Kind = http.StatusOK, ""
	if admitted && h.cfg.RecordResult != nil {
		// A successful decision must reach the local audit file before the
		// response. Delivery failures still appear in the operational audit.
		recordAttempted = true
		telemetry.FinishAuditEvent(rec, rec.StatusCode, rec.Kind, time.Since(start).Milliseconds())
		if err := h.recordResult(ctx, rec, call, decoded, result, true); err != nil {
			fail(w, rec, http.StatusServiceUnavailable, llmtypes.KindUnknown, "could not save the TypeSafe result")
			return
		}
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = http.NewResponseController(w).SetWriteDeadline(deadline)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(result); err != nil {
		rec.Kind = llmtypes.KindClientClosed
	}
}

func needsPayloadSlot(r *http.Request) bool {
	encoding := r.Header.Get("Content-Encoding")
	return r.ContentLength < 0 || r.ContentLength > guardedRequestBytes ||
		(encoding != "" && !strings.EqualFold(encoding, "identity")) ||
		strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type"))), "multipart/") ||
		r.Header.Get("TypeSafe-Danger-Zone-Images") == "true"
}

func (h *Handler) recordResult(ctx context.Context, rec *telemetry.AuditEvent, call *telemetry.CallEvent, request decisionRequest, response json.RawMessage, acknowledged bool) error {
	if h.results == nil && (!acknowledged || h.cfg.RecordResult == nil) {
		return nil
	}
	event, ok := llmresultschema.FromDecision(llmresultschema.DecisionBuildInput{
		Audit: rec, Call: call, Request: request.auditBody(), RequestHeaders: request.headers, Response: response,
	})
	if !ok {
		return nil
	}
	if acknowledged && h.cfg.RecordResult != nil {
		if err := h.cfg.RecordResult(ctx, event); err != nil {
			h.log.WarnContext(ctx, "system one audit record failed", "request_id", rec.RequestID, "err", err.Error())
			return err
		}
	} else {
		h.results.Emit(ctx, event)
	}
	return nil
}

func fail(w http.ResponseWriter, rec *telemetry.AuditEvent, status int, kind llmtypes.ErrorKind, message string) {
	rec.StatusCode, rec.Kind = status, kind
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"message": message, "type": kind, "code": nil, "status": status,
	}})
}
