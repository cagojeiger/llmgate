package server

import (
	"encoding/json"
	"net/http"
	"time"

	"llmgate/internal/llmtypes"
	"llmgate/internal/telemetry"
)

func newFinalizedEvent(common telemetry.EventCommon, modelRequested string, requestRaw []byte, stream bool) *telemetry.LLMCallFinalizedEvent {
	event := telemetry.NewLLMCallFinalizedEvent(common)
	event.Routing.ModelRequested = modelRequested
	event.Request = telemetry.RawEnvelope{
		Available: true,
		RawJSON:   cloneRawJSON(requestRaw),
	}
	event.Response = telemetry.RawEnvelope{
		Available: false,
		Reason:    "not_finalized",
	}
	event.Stream = telemetry.StreamEnvelope{Enabled: stream}
	return event
}

func finishFinalizedEvent(event *telemetry.LLMCallFinalizedEvent, rec *telemetry.AuditEvent, call *telemetry.CallEvent) {
	if event == nil || rec == nil {
		return
	}
	event.Operation = rec.Operation
	event.Status = finalizedStatus(rec, call)
	event.CompletedAt = time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
	event.DurationMS = rec.DurationMS
	if call != nil {
		event.Usage = cloneUsage(call.Usage)
		if !event.Response.Available && call.Kind != "" && event.Response.Reason == "" {
			event.Response.Reason = "response_unavailable"
		}
	}
	if rec.Kind != "" {
		if event.Error == nil {
			event.Error = &telemetry.EventError{Kind: string(rec.Kind)}
		} else if event.Error.Kind == "" {
			event.Error.Kind = string(rec.Kind)
		}
	}
}

func adoptFinalizedRoute(event *telemetry.LLMCallFinalizedEvent, call *telemetry.CallEvent) {
	if event == nil || call == nil {
		return
	}
	event.Routing.ModelRequested = call.ModelRequested
	event.Routing.ModelUsed = call.ModelUsed
	event.Routing.Vendor = call.Vendor
	if len(call.Attempts) > 0 {
		event.Routing.Attempts = make([]telemetry.EventAttempt, len(call.Attempts))
		for i, attempt := range call.Attempts {
			event.Routing.Attempts[i] = telemetry.EventAttempt{
				Vendor:     attempt.Vendor,
				Model:      attempt.Model,
				DurationMS: attempt.DurationMS,
				StatusCode: attempt.StatusCode,
				Kind:       string(attempt.Kind),
				Usage:      cloneUsage(attempt.Usage),
				VendorCost: attempt.VendorCost,
			}
			if !attempt.StartedAt.IsZero() {
				event.Routing.Attempts[i].StartedAt = attempt.StartedAt.UTC().Format("2006-01-02T15:04:05.000000000Z")
			}
		}
	}
}

func setFinalizedResponse(event *telemetry.LLMCallFinalizedEvent, raw []byte) {
	if event == nil {
		return
	}
	event.Response = telemetry.RawEnvelope{
		Available: true,
		RawJSON:   cloneRawJSON(raw),
	}
}

func markFinalizedResponseUnavailable(event *telemetry.LLMCallFinalizedEvent, reason string, err error) {
	if event == nil {
		return
	}
	event.Response = telemetry.RawEnvelope{Available: false, Reason: reason}
	if err != nil {
		event.Error = &telemetry.EventError{
			Kind:    string(llmtypes.ErrorKindOf(err)),
			Message: err.Error(),
		}
	}
}

func finalizeStreamPayload(event *telemetry.LLMCallFinalizedEvent, capture *streamCapture, summary *llmtypes.Summary, kind llmtypes.ErrorKind) {
	if event == nil || capture == nil {
		return
	}
	events := capture.Events()
	event.Stream.EventsAvailable = len(events) > 0
	event.Stream.Events = events
	raw, err := capture.BuildResponse(summary)
	if err != nil {
		markFinalizedResponseUnavailable(event, "stream_reassembly_failed", err)
		return
	}
	if len(raw) > 0 && kind == "" {
		setFinalizedResponse(event, raw)
		return
	}
	if len(raw) > 0 {
		event.Response = telemetry.RawEnvelope{
			Available: true,
			RawJSON:   raw,
			Reason:    "partial_stream_response",
		}
		return
	}
	if event.Response.Reason == "" || event.Response.Reason == "not_finalized" {
		event.Response = telemetry.RawEnvelope{Available: false, Reason: "stream_response_unavailable"}
	}
}

func finalizedStatus(rec *telemetry.AuditEvent, call *telemetry.CallEvent) telemetry.LLMCallStatus {
	if rec.Kind == llmtypes.KindClientClosed {
		return telemetry.LLMCallStatusClientClosed
	}
	if rec.Kind != "" {
		if call != nil && (call.ResponseBytes > 0 || call.StreamChunks > 0) {
			return telemetry.LLMCallStatusPartial
		}
		return telemetry.LLMCallStatusError
	}
	if rec.StatusCode >= http.StatusOK && rec.StatusCode < http.StatusMultipleChoices {
		return telemetry.LLMCallStatusSuccess
	}
	return telemetry.LLMCallStatusError
}

func cloneRawJSON(raw []byte) json.RawMessage {
	if raw == nil {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func cloneUsage(usage *llmtypes.Usage) *llmtypes.Usage {
	if usage == nil {
		return nil
	}
	cp := *usage
	return &cp
}
