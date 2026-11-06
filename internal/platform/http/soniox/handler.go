// Package soniox brokers the native Soniox realtime speech-to-text protocol.
package soniox

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	llmresultschema "llmgate/internal/domain/llmresult/schema"
	llmresultsink "llmgate/internal/domain/llmresult/sink"
	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/telemetry"
	httpauth "llmgate/internal/platform/http/auth"
	"llmgate/internal/platform/http/requestid"
	"llmgate/internal/platform/http/response"
)

const (
	Model           = "stt-rt-v6"
	DefaultEndpoint = "wss://stt-rt.soniox.com/transcribe-websocket"
	operation       = "audio.soniox.realtime.transcription"
)

// Capture records one ordered stream of Soniox-facing application messages.
// Append is called serially. Finish releases the capture even on error.
type Capture interface {
	ID() string
	Append(direction, kind string, payload []byte) error
	Finish(complete bool, reason string) error
}

type Config struct {
	APIKey         string
	Endpoint       string
	ServiceVersion string
	Environment    string
	ResultSink     llmresultsink.Sink
	BeginCapture   func(requestID, model string) (Capture, error)
}

type Handler struct {
	cfg    Config
	log    *slog.Logger
	events telemetry.EventSink
	client *http.Client

	mu       sync.Mutex
	active   sync.WaitGroup
	sessions map[uint64]context.CancelFunc
	sequence uint64
	done     chan struct{}
}

func NewHandler(log *slog.Logger, events telemetry.EventSink, cfg Config) *Handler {
	if log == nil {
		log = slog.Default()
	}
	if events == nil {
		events = telemetry.NopSink{}
	}
	if cfg.ResultSink != nil {
		cfg.ResultSink = llmresultsink.NewRecoveringSink(cfg.ResultSink, log)
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = DefaultEndpoint
	}
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	if cfg.ServiceVersion == "" {
		cfg.ServiceVersion = "dev"
	}
	if cfg.Environment == "" {
		cfg.Environment = "local"
	}
	return &Handler{
		cfg: cfg, log: log, events: telemetry.NewRecoveringSink(events, log),
		client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		sessions: make(map[uint64]context.CancelFunc),
	}
}

// Shutdown closes hijacked connections before the archive and audit sinks shut down.
func (h *Handler) Shutdown(ctx context.Context) error {
	h.mu.Lock()
	if h.done == nil {
		for _, cancel := range h.sessions {
			cancel()
		}
		h.done = make(chan struct{})
		go func() {
			h.active.Wait()
			close(h.done)
		}()
	}
	done := h.done
	h.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Handler) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return h.Shutdown(ctx)
}

func (h *Handler) track(ctx context.Context) (context.Context, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.done != nil {
		return nil, nil
	}
	// Hijacked connections have a separate lifetime from the HTTP request.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), maxSessionDuration)
	h.sequence++
	id := h.sequence
	h.sessions[id] = cancel
	h.active.Add(1)
	return ctx, func() {
		cancel()
		h.mu.Lock()
		delete(h.sessions, id)
		h.mu.Unlock()
		h.active.Done()
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestCtx := r.Context()
	sessionCtx, finish := h.track(requestCtx)
	if finish != nil {
		defer finish()
	}

	start := time.Now()
	consumer := httpauth.FromContext(requestCtx)
	common := telemetry.NewEventCommon(telemetry.CommonInput{
		Timestamp: start, RequestID: requestid.FromContext(requestCtx),
		ServiceVersion: h.cfg.ServiceVersion, Environment: h.cfg.Environment,
		Operation: operation, ConsumerName: consumer.Name, ConsumerKeyID: consumer.KeyID,
	})
	rec := telemetry.NewAuditEvent(common)
	rec.StatusCode, rec.Kind = http.StatusInternalServerError, llmtypes.KindPanic
	telemetry.SetResource(rec, "llm_model", Model)
	var result sessionResult
	defer func() {
		telemetry.FinishAuditEvent(rec, rec.StatusCode, rec.Kind, time.Since(start).Milliseconds())
		auditCtx := context.WithoutCancel(requestCtx)
		h.events.Emit(auditCtx, rec)
		if h.cfg.ResultSink != nil {
			event, _ := llmresultschema.FromRealtime(llmresultschema.RealtimeBuildInput{
				Audit: rec, ModelRequested: Model, ModelUsed: result.model,
			})
			event.Vendor = "soniox"
			event.SonioxCaptureID = result.captureID
			if result.captureID != "" {
				event.SonioxCaptureComplete = &result.complete
			}
			h.cfg.ResultSink.Emit(auditCtx, event)
		}
	}()

	if consumer.AuthError != "" || consumer.KeyID == "" {
		authErr := consumer.AuthError
		if authErr == "" {
			authErr = telemetry.AuthErrorMissing
		}
		telemetry.MarkAuthFailure(rec, authErr)
		failHTTP(w, rec, llmtypes.KindAuth, "unauthorized")
		return
	}
	telemetry.MarkAuthSuccess(rec)
	if len(consumer.AllowedAliases) > 0 && !slices.ContainsFunc(consumer.AllowedAliases, func(id string) bool {
		return strings.EqualFold(id, Model)
	}) {
		telemetry.MarkPolicyDenied(rec, telemetry.DenyReasonModelNotAllowed)
		failHTTP(w, rec, llmtypes.KindForbidden, "model not allowed")
		return
	}
	telemetry.MarkPolicyAllowed(rec)

	if finish == nil || h.cfg.APIKey == "" || h.cfg.BeginCapture == nil || !validEndpoint(h.cfg.Endpoint) {
		rec.StatusCode, rec.Kind = http.StatusServiceUnavailable, llmtypes.KindUpstream
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rec.StatusCode)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"message": "Soniox realtime is unavailable", "type": rec.Kind, "code": nil, "status": rec.StatusCode,
		}})
		return
	}
	client, err := websocket.Accept(w, r, nil)
	if err != nil {
		rec.StatusCode, rec.Kind = http.StatusBadRequest, llmtypes.KindBadRequest
		return
	}
	defer func() { _ = client.CloseNow() }()

	result = h.session(sessionCtx, client, rec.RequestID)
	rec.StatusCode, rec.Kind = result.status, result.kind
	if result.reason != "finished" {
		h.log.InfoContext(context.WithoutCancel(requestCtx), "Soniox realtime ended", "request_id", rec.RequestID, "reason", result.reason)
	}
}

func validEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return false
	}
	if u.Scheme == "wss" {
		return true
	}
	// Only a local mock may receive a bearer over plain WebSocket.
	return u.Scheme == "ws" && (u.Hostname() == "localhost" || net.ParseIP(u.Hostname()).IsLoopback())
}

func failHTTP(w http.ResponseWriter, rec *telemetry.AuditEvent, kind llmtypes.ErrorKind, message string) {
	err := &llmtypes.Error{Kind: kind, Message: message}
	rec.StatusCode, rec.Kind = response.Status(err), kind
	response.WriteError(w, err)
}
