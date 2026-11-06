package schema

import (
	"bytes"
	"encoding/json"

	"llmgate/internal/domain/telemetry"
)

type DecisionBuildInput struct {
	Audit    *telemetry.AuditEvent
	Call     *telemetry.CallEvent
	Request  json.RawMessage
	Response json.RawMessage
}

func FromDecision(in DecisionBuildInput) (*Event, bool) {
	ev, ok := FromTelemetry(BuildInput{Audit: in.Audit, Call: in.Call})
	if !ok {
		return nil, false
	}
	// The async sink can encode the result after the handler returns.
	ev.DecisionRequest = bytes.Clone(in.Request)
	ev.DecisionResponse = bytes.Clone(in.Response)
	return ev, true
}
