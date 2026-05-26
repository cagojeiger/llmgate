package schema

import (
	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/telemetry"
)

type BuildInput struct {
	Audit       *telemetry.AuditEvent
	Call        *telemetry.CallEvent
	Request     *llmtypes.Request
	Response    *llmtypes.Response
	PayloadMode PayloadMode
}

// FromTelemetry builds the finalized analytics event from already-finalized
// telemetry records plus the original OpenAI-shaped request/response bodies.
func FromTelemetry(in BuildInput) (*Event, bool) {
	if in.Audit == nil || !telemetry.CallAttempted(in.Call) {
		return nil, false
	}
	payloadMode, err := ParsePayloadMode(string(in.PayloadMode))
	if err != nil {
		payloadMode = PayloadModeMetadataOnly
	}
	call := in.Call
	audit := in.Audit

	ev := &Event{
		SchemaVersion: SchemaVersion,
		EventType:     EventType,
		PayloadMode:   string(payloadMode),

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
	switch payloadMode {
	case PayloadModeFull:
		ev.Request = cloneJSON(in.Request)
		ev.Response = cloneJSON(in.Response)
	case PayloadModeRedacted:
		ev.Request = redactRequest(in.Request)
		ev.Response = redactResponse(in.Response)
	case PayloadModeMetadataOnly:
		// Request and response bodies intentionally omitted.
	}
	return ev, true
}

const redactedString = "[redacted]"

func redactRequest(in *llmtypes.Request) *llmtypes.Request {
	if in == nil {
		return nil
	}
	out := &llmtypes.Request{
		Model:       in.Model,
		MaxTokens:   in.MaxTokens,
		Temperature: cloneFloat64(in.Temperature),
		TopP:        cloneFloat64(in.TopP),
		Seed:        cloneInt(in.Seed),
		Stream:      cloneBool(in.Stream),
	}
	if in.User != "" {
		out.User = redactedString
	}
	if len(in.Messages) > 0 {
		out.Messages = make([]llmtypes.Message, len(in.Messages))
		for i, msg := range in.Messages {
			out.Messages[i] = redactMessage(msg)
		}
	}
	return out
}

func redactResponse(in *llmtypes.Response) *llmtypes.Response {
	if in == nil {
		return nil
	}
	out := &llmtypes.Response{
		ID:                in.ID,
		Object:            in.Object,
		Created:           in.Created,
		Model:             in.Model,
		SystemFingerprint: in.SystemFingerprint,
		Usage:             in.Usage.Clone(),
	}
	if len(in.Choices) > 0 {
		out.Choices = make([]llmtypes.Choice, len(in.Choices))
		for i, choice := range in.Choices {
			out.Choices[i] = llmtypes.Choice{
				Index:        choice.Index,
				Message:      redactMessage(choice.Message),
				FinishReason: choice.FinishReason,
			}
		}
	}
	return out
}

func redactMessage(in llmtypes.Message) llmtypes.Message {
	out := llmtypes.Message{Role: in.Role}
	if in.Content != "" || len(in.ContentRaw) > 0 {
		out.Content = redactedString
	}
	if in.ReasoningContent != "" {
		out.ReasoningContent = redactedString
	}
	return out
}

func cloneBool(in *bool) *bool {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneFloat64(in *float64) *float64 {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneInt(in *int) *int {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}
