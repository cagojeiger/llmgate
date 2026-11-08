package schema

import (
	"bytes"
	"encoding/json"
	"maps"

	"llmgate/internal/domain/telemetry"
)

type DecisionBuildInput struct {
	Audit          *telemetry.AuditEvent
	Call           *telemetry.CallEvent
	Request        json.RawMessage
	RequestHeaders map[string]string
	Response       json.RawMessage
}

func FromDecision(in DecisionBuildInput) (*Event, bool) {
	ev, ok := FromTelemetry(BuildInput{Audit: in.Audit, Call: in.Call})
	if !ok {
		return nil, false
	}
	// The async sink can encode the result after the handler returns.
	ev.DecisionRequest = bytes.Clone(in.Request)
	ev.DecisionRequestHeaders = maps.Clone(in.RequestHeaders)
	ev.DecisionResponse = bytes.Clone(in.Response)
	return ev, true
}
