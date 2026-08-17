// Package realtime serves the OpenAI /v1/realtime WebSocket route. It is the
// mode-3 counterpart of the HTTP /v1/audio/transcriptions handler: instead of
// routing one request through the circuit-breaker Service, it upgrades the
// client to a WebSocket and transparently brokers an OpenAI Realtime
// transcription session to a registered upstream realtime STT server, relaying
// event frames verbatim in both directions.
//
// The gateway never reinterprets the protocol — session.update /
// input_audio_buffer.* frames flow client→upstream and session.* /
// conversation.item.input_audio_transcription.* frames flow upstream→client
// unchanged. It only sniffs the upstream stream for completed-transcription
// frames so the session can be audited when it ends.
package realtime

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"llmgate/internal/domain/catalog"
	llmresultschema "llmgate/internal/domain/llmresult/schema"
	llmresultsink "llmgate/internal/domain/llmresult/sink"
	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/telemetry"
	httpauth "llmgate/internal/platform/http/auth"
	"llmgate/internal/platform/http/requestid"
	"llmgate/internal/platform/http/response"
)

// defaultRealtimeAlias is used when the client omits ?model=. It mirrors the
// HTTP transcription default (alias "stt") for the realtime surface.
const defaultRealtimeAlias = "stt-realtime"

type Handler struct {
	catalog        *catalog.Catalog
	log            *slog.Logger
	events         telemetry.EventSink
	results        llmresultsink.Sink
	serviceVersion string
	environment    string
}

type HandlerConfig struct {
	ServiceVersion string
	Environment    string
	ResultSink     llmresultsink.Sink
}

func NewHandler(cat *catalog.Catalog, log *slog.Logger, events telemetry.EventSink, cfg HandlerConfig) *Handler {
	if log == nil {
		log = slog.Default()
	}
	if events == nil {
		events = telemetry.NopSink{}
	}
	events = telemetry.NewRecoveringSink(events, log)
	// A nil or no-op ResultSink leaves h.results nil so the session-close defer
	// skips building the result event entirely — mirrors the audio handler.
	var results llmresultsink.Sink
	if cfg.ResultSink != nil {
		if _, nop := cfg.ResultSink.(llmresultsink.NopSink); !nop {
			results = llmresultsink.NewRecoveringSink(cfg.ResultSink, log)
		}
	}
	serviceVersion := cfg.ServiceVersion
	if serviceVersion == "" {
		serviceVersion = "dev"
	}
	environment := cfg.Environment
	if environment == "" {
		environment = "local"
	}
	return &Handler{
		catalog:        cat,
		log:            log,
		events:         events,
		results:        results,
		serviceVersion: serviceVersion,
		environment:    environment,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ctx := r.Context()
	h.serve(w, r, ctx, start)
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, ctx context.Context, start time.Time) {
	consumer := httpauth.FromContext(ctx)
	requested := r.URL.Query().Get("model")
	if requested == "" {
		requested = defaultRealtimeAlias
	}
	common := telemetry.NewEventCommon(telemetry.CommonInput{
		Timestamp:      start,
		RequestID:      requestid.FromContext(ctx),
		ServiceVersion: h.serviceVersion,
		Environment:    h.environment,
		Operation:      "audio.realtime.transcription",
		ConsumerName:   consumer.Name,
		ConsumerKeyID:  consumer.KeyID,
	})
	rec := telemetry.NewAuditEvent(common)
	telemetry.MarkAuthSuccess(rec)
	telemetry.SetResource(rec, "llm_model", requested)

	var modelUsed, endpoint string
	var turns int
	var transcripts []string

	// One session-level record at close: the operational audit event plus the
	// finalized result event. Realtime is session-scoped and per-turn audio is
	// never buffered, so the durable record carries the accumulated
	// transcript(s) + counts + duration, not the raw audio.
	defer func() {
		telemetry.FinishAuditEvent(rec, rec.StatusCode, rec.Kind, time.Since(start).Milliseconds())
		h.events.Emit(ctx, rec)
		if h.results != nil {
			if ev, ok := llmresultschema.FromRealtime(llmresultschema.RealtimeBuildInput{
				Audit:          rec,
				ModelRequested: requested,
				ModelUsed:      modelUsed,
				Endpoint:       endpoint,
				Turns:          turns,
				Transcripts:    transcripts,
			}); ok {
				h.results.Emit(ctx, ev)
			}
		}
	}()

	if consumer.AuthError != "" {
		telemetry.MarkAuthFailure(rec, consumer.AuthError)
		perr := &llmtypes.Error{Kind: llmtypes.KindAuth, Message: "unauthorized"}
		adoptError(rec, perr)
		response.WriteError(w, perr)
		return
	}
	if !modelAllowed(requested, consumer.AllowedAliases) {
		telemetry.MarkPolicyDenied(rec, telemetry.DenyReasonModelNotAllowed)
		perr := &llmtypes.Error{Kind: llmtypes.KindForbidden, Message: "model not allowed"}
		adoptError(rec, perr)
		response.WriteError(w, perr)
		return
	}
	telemetry.MarkPolicyAllowed(rec)

	// Resolve before upgrading: an unknown alias / missing realtime endpoint is
	// a plain HTTP error, so it must be written before the WebSocket handshake
	// hijacks the connection.
	used, base, err := h.resolve(requested)
	if err != nil {
		adoptError(rec, err)
		response.WriteError(w, err)
		return
	}
	modelUsed, endpoint = used, base

	// Programmatic realtime clients (server-side SDKs) send no Origin header, so
	// the default same-origin check would pass; InsecureSkipVerify keeps the
	// gateway usable from any origin since the bearer already authenticated the
	// caller on the upgrade GET.
	clientConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		// Accept already wrote the HTTP error response.
		rec.Kind = llmtypes.KindBadRequest
		rec.StatusCode = http.StatusBadRequest
		h.log.LogAttrs(ctx, slog.LevelInfo, "realtime client upgrade failed",
			slog.String("request_id", rec.RequestID),
			slog.String("err", err.Error()),
		)
		return
	}
	clientConn.SetReadLimit(maxRealtimeMessageBytes)

	// broker intentionally detaches from the request ctx: the connection is
	// hijacked by the upgrade, so the relay is governed by its own session ctx
	// (see broker) rather than the now-canceling request ctx.
	//nolint:contextcheck // WS relay outlives the hijacked request ctx by design
	turns, transcripts = h.broker(clientConn, endpoint, rec)
}

// resolve maps the requested alias/model to the concrete realtime model id and
// its upstream ws base_url. It mirrors routing.resolveTranscribeChain (alias →
// chain, else raw model) but filters to realtime-api models and returns the
// base_url directly — realtime endpoints never enter the routing Service.
func (h *Handler) resolve(model string) (modelUsed, baseURL string, err error) {
	if h.catalog == nil {
		return "", "", &llmtypes.Error{Kind: llmtypes.KindUpstream, Message: "no realtime catalog configured"}
	}
	key := strings.ToLower(model)
	var chain []string
	if a, ok := h.catalog.Aliases[key]; ok {
		chain = a.Chain
	} else if _, ok := h.catalog.Models[key]; ok {
		chain = []string{key}
	} else {
		return "", "", &llmtypes.Error{Kind: llmtypes.KindBadRequest, Message: "unknown model: " + model}
	}
	for _, id := range chain {
		m, ok := h.catalog.Models[strings.ToLower(id)]
		if !ok || m.API != catalog.APIRealtime {
			continue
		}
		return m.ID, m.BaseURL, nil
	}
	return "", "", &llmtypes.Error{Kind: llmtypes.KindUpstream, Message: "no realtime endpoint available for model: " + model}
}

// adoptError populates rec.Kind and rec.StatusCode from err.
func adoptError(rec *telemetry.AuditEvent, err error) {
	rec.Kind = llmtypes.ErrorKindOf(err)
	rec.StatusCode = response.Status(err)
}

// modelAllowed mirrors the audio handler's per-consumer allowlist check: an
// empty allowlist means "any model", otherwise the requested alias must match.
func modelAllowed(model string, allowed []string) bool {
	if model == "" || len(allowed) == 0 {
		return true
	}
	for _, alias := range allowed {
		if strings.EqualFold(model, alias) {
			return true
		}
	}
	return false
}
