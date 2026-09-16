package schema

import (
	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/telemetry"
)

type EmbeddingBuildInput struct {
	Audit    *telemetry.AuditEvent
	Call     *telemetry.CallEvent
	Request  *llmtypes.EmbeddingRequest
	Response *llmtypes.EmbeddingResponse
}

func FromEmbedding(in EmbeddingBuildInput) (*Event, bool) {
	ev, ok := FromTelemetry(BuildInput{Audit: in.Audit, Call: in.Call})
	if !ok {
		return nil, false
	}
	ev.EmbeddingRequest = cloneJSON(in.Request)
	ev.EmbeddingResponse = cloneJSON(in.Response)
	return ev, true
}
