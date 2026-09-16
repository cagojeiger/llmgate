package workers

import (
	"encoding/json"
	"io"
	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/telemetry"
	"llmgate/internal/platform/http/auth"
	"llmgate/internal/platform/http/requestid"
	"llmgate/internal/platform/relaytoken"
	"net/http"
	"slices"
	"time"
)

type Handler struct {
	issuer               *relaytoken.Issuer
	events               telemetry.EventSink
	version, environment string
}

func New(issuer *relaytoken.Issuer, events telemetry.EventSink, version, environment string) *Handler {
	return &Handler{issuer: issuer, events: events, version: version, environment: environment}
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	consumer := auth.FromContext(r.Context())
	status := http.StatusInternalServerError
	rec := telemetry.NewAuditEvent(telemetry.NewEventCommon(telemetry.CommonInput{Timestamp: start, RequestID: requestid.FromContext(r.Context()), ServiceVersion: h.version, Environment: h.environment, Operation: "worker.token", ConsumerName: consumer.Name, ConsumerKeyID: consumer.KeyID}))
	defer func() {
		telemetry.FinishAuditEvent(rec, status, rec.Kind, time.Since(start).Milliseconds())
		h.events.Emit(r.Context(), rec)
	}()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	fail := func(code int, kind llmtypes.ErrorKind, message string) {
		status = code
		rec.Kind = kind
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": message, "type": string(kind)}})
	}
	if consumer.Name == "" || consumer.AuthError != "" {
		telemetry.MarkAuthFailure(rec, consumer.AuthError)
		fail(401, llmtypes.KindAuth, "unauthorized")
		return
	}
	telemetry.MarkAuthSuccess(rec)
	var input struct {
		ProtocolVersion int    `json:"protocol_version"`
		Profile         string `json:"profile"`
		ProfileVersion  string `json:"profile_version"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(&input) != nil || d.Decode(new(any)) != io.EOF || input.ProtocolVersion != 1 {
		fail(400, llmtypes.KindBadRequest, "invalid worker token request")
		return
	}
	telemetry.SetResource(rec, "worker_profile", input.Profile)
	if !slices.Contains(consumer.AllowedWorkerProfiles, input.Profile) {
		telemetry.MarkPolicyDenied(rec, telemetry.DenyReasonModelNotAllowed)
		fail(403, llmtypes.KindForbidden, "worker profile not allowed")
		return
	}
	telemetry.MarkPolicyAllowed(rec)
	grant, err := h.issuer.Issue(input.Profile, input.ProfileVersion, time.Now())
	if err != nil {
		fail(400, llmtypes.KindBadRequest, "unsupported worker profile/version")
		return
	}
	status = 200
	_ = json.NewEncoder(w).Encode(grant) // #nosec G117 -- this authenticated no-store endpoint intentionally returns the scoped access token.
}
