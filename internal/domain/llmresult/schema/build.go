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

		Request:  cloneRequest(in.Request),
		Response: cloneResponse(in.Response),
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
