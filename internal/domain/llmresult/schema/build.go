package schema

import (
	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/telemetry"
)

type BuildInput struct {
	Audit    *telemetry.AuditEvent
	Call     *telemetry.CallEvent
	Request  *llmtypes.Request
	Response *llmtypes.Response
}

// FromTelemetry builds the finalized analytics event from already-finalized
// telemetry records plus the original OpenAI-shaped request/response bodies.
func FromTelemetry(in BuildInput) (*Event, bool) {
	if in.Audit == nil || !telemetry.CallAttempted(in.Call) {
		return nil, false
	}
	call := in.Call
	audit := in.Audit

	ev := &Event{
		SchemaVersion: SchemaVersion,
		EventType:     EventType,

		Timestamp:      audit.Timestamp,
		RequestID:      audit.RequestID,
		ServiceName:    audit.ServiceName,
		ServiceVersion: audit.ServiceVersion,
		Environment:    audit.Environment,
		Operation:      audit.Operation,
		ConsumerName:   audit.ConsumerName,
		ConsumerKeyID:  audit.ConsumerKeyID,

		StatusCode: audit.StatusCode,
		ErrorKind:  audit.Kind,
		DurationMS: audit.DurationMS,

		Request:  cloneJSON(in.Request),
		Response: cloneJSON(in.Response),
		Usage:    call.Usage.Clone(),
		Attempts: cloneAttempts(call.Attempts),

		ModelRequested: call.ModelRequested,
		ModelUsed:      call.ModelUsed,
		Vendor:         call.Vendor,
		RequestBytes:   call.RequestBytes,
		ResponseBytes:  call.ResponseBytes,
		VendorCost:     call.VendorCost,
		FirstByteMS:    call.FirstByteMS,
		StreamChunks:   call.StreamChunks,
	}
	return ev, true
}

// TranscriptionBuildInput is the transcription counterpart of BuildInput: the
// finalized telemetry records plus the typed audio request/response bodies.
type TranscriptionBuildInput struct {
	Audit    *telemetry.AuditEvent
	Call     *telemetry.CallEvent
	Request  *llmtypes.TranscriptionRequest
	Response *llmtypes.TranscriptionResponse
}

// FromTranscription builds the finalized analytics event for a transcription
// request. It mirrors FromTelemetry — same common fields and routing/accounting
// data — but populates the Transcription* payload fields instead of the chat
// Request/Response, so both surfaces flow through the one ResultSink.
func FromTranscription(in TranscriptionBuildInput) (*Event, bool) {
	if in.Audit == nil || !telemetry.CallAttempted(in.Call) {
		return nil, false
	}
	call := in.Call
	audit := in.Audit

	ev := &Event{
		SchemaVersion: SchemaVersion,
		EventType:     EventType,

		Timestamp:      audit.Timestamp,
		RequestID:      audit.RequestID,
		ServiceName:    audit.ServiceName,
		ServiceVersion: audit.ServiceVersion,
		Environment:    audit.Environment,
		Operation:      audit.Operation,
		ConsumerName:   audit.ConsumerName,
		ConsumerKeyID:  audit.ConsumerKeyID,

		StatusCode: audit.StatusCode,
		ErrorKind:  audit.Kind,
		DurationMS: audit.DurationMS,

		TranscriptionRequest:  cloneJSON(in.Request),
		TranscriptionResponse: cloneJSON(in.Response),
		Attempts:              cloneAttempts(call.Attempts),

		ModelRequested: call.ModelRequested,
		ModelUsed:      call.ModelUsed,
		Vendor:         call.Vendor,
		RequestBytes:   call.RequestBytes,
		ResponseBytes:  call.ResponseBytes,
	}
	return ev, true
}

// RealtimeBuildInput is the realtime counterpart of BuildInput. A realtime
// session has no per-attempt CallEvent (it is a verbatim WS relay, not a
// circuit-broken vendor call), so the summary fields arrive directly rather
// than being lifted off a Call. Audit is the already-finalized session record
// (status, error kind, and DurationMS = the full session duration).
type RealtimeBuildInput struct {
	Audit          *telemetry.AuditEvent
	ModelRequested string
	ModelUsed      string
	Endpoint       string
	Turns          int
	Transcripts    []string
}

// FromRealtime builds the finalized analytics event for one realtime
// transcription session. It mirrors FromTelemetry's common/routing fields but
// populates the Realtime* summary instead of a chat or audio body: the
// accumulated transcript(s), completed-turn count, and upstream endpoint. It
// needs no CallEvent — a session that never reached the accept/dial stage still
// carries a meaningful audit record — so the only gate is a non-nil Audit.
func FromRealtime(in RealtimeBuildInput) (*Event, bool) {
	if in.Audit == nil {
		return nil, false
	}
	audit := in.Audit

	ev := &Event{
		SchemaVersion: SchemaVersion,
		EventType:     EventType,

		Timestamp:      audit.Timestamp,
		RequestID:      audit.RequestID,
		ServiceName:    audit.ServiceName,
		ServiceVersion: audit.ServiceVersion,
		Environment:    audit.Environment,
		Operation:      audit.Operation,
		ConsumerName:   audit.ConsumerName,
		ConsumerKeyID:  audit.ConsumerKeyID,

		StatusCode: audit.StatusCode,
		ErrorKind:  audit.Kind,
		DurationMS: audit.DurationMS,

		RealtimeEndpoint:    in.Endpoint,
		RealtimeTurns:       in.Turns,
		RealtimeTranscripts: append([]string(nil), in.Transcripts...),

		ModelRequested: in.ModelRequested,
		ModelUsed:      in.ModelUsed,
	}
	return ev, true
}
