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
)

type HandlerConfig struct {
	APIKey              string
	Endpoint            string
	ServiceVersion      string
	Environment         string
	RequestTimeout      time.Duration
	MaxRequestBodyBytes int64
	ResultSink          llmresultsink.Sink
}

type Handler struct {
	cfg     HandlerConfig
	client  *http.Client
	events  telemetry.EventSink
	results llmresultsink.Sink
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
	if cfg.MaxRequestBodyBytes <= 0 {
		cfg.MaxRequestBodyBytes = 10 << 20
	}
	cfg.MaxRequestBodyBytes = min(cfg.MaxRequestBodyBytes, maxTypeSafeRequestBytes)
	client := upstream.DefaultClient()
	// Even a same-host redirect can re-submit a billable POST.
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &Handler{cfg: cfg, client: client, events: telemetry.NewRecoveringSink(events, log), results: results}
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
	var request, result json.RawMessage
	defer func() {
		telemetry.FinishAuditEvent(rec, rec.StatusCode, rec.Kind, time.Since(start).Milliseconds())
		h.events.Emit(ctx, rec)
		if !telemetry.CallAttempted(call) {
			return
		}
		telemetry.FinishCallFromAudit(call, rec)
		h.events.Emit(ctx, call)
		if h.results != nil {
			if ev, ok := llmresultschema.FromDecision(llmresultschema.DecisionBuildInput{
				Audit: rec, Call: call, Request: request, Response: result,
			}); ok {
				h.results.Emit(ctx, ev)
			}
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
	decoded, problem := decodeRequest(w, r, h.cfg.MaxRequestBodyBytes)
	if problem != nil {
		fail(w, rec, problem.status, llmtypes.KindBadRequest, problem.message)
		return
	}
	request = decoded.body
	call = telemetry.NewCallEvent(common, Model, decoded.inputBytes)
	// Keep the completed decision even if the caller disconnects during delivery.
	result = h.evaluate(w, ctx, rec, call, decoded)
	if result == nil {
		return
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = http.NewResponseController(w).SetWriteDeadline(deadline)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	rec.StatusCode, rec.Kind = http.StatusOK, ""
	if _, err := w.Write(result); err != nil {
		rec.Kind = llmtypes.KindClientClosed
	}
}

func fail(w http.ResponseWriter, rec *telemetry.AuditEvent, status int, kind llmtypes.ErrorKind, message string) {
	rec.StatusCode, rec.Kind = status, kind
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"message": message, "type": kind, "code": nil, "status": status,
	}})
}
