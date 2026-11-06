package schema

import (
	"encoding/json"
	"net/http"
	"testing"

	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/telemetry"
)

func TestFromDecision_StoresStructuredPayloads(t *testing.T) {
	const request = `{"model":"jev-1.14.0","state":{"text":"Please cancel my plan"},"questions":{"cancel":{"type":"noul","instructions":"Is this a cancellation?"}}}`
	const response = `{"model":"jev-1.14.0","answers":{"cancel":{"type":"noul","noul":0.98}},"usage":{"input_tokens":15,"output_tokens":2}}`
	audit := &telemetry.AuditEvent{EventCommon: telemetry.EventCommon{
		RequestID:    "req-1",
		ConsumerName: "alpha",
		StatusCode:   http.StatusOK,
	}}
	call := &telemetry.CallEvent{
		ModelRequested: "jev-1.14.0",
		ModelUsed:      "jev-1.14.0",
		Vendor:         "typesafe",
		Usage:          &llmtypes.Usage{TotalTokens: 17},
		Attempts:       []llmtypes.Attempt{{Vendor: "typesafe", StatusCode: http.StatusOK}},
	}
	input := DecisionBuildInput{Audit: audit, Call: call, Request: json.RawMessage(request), Response: json.RawMessage(response)}
	event, ok := FromDecision(input)
	if !ok {
		t.Fatal("FromDecision returned no event for an attempted request")
	}
	input.Request[0] = 'X'
	input.Response[0] = 'X'
	call.Usage.TotalTokens = 0
	call.Attempts[0].StatusCode = http.StatusBadGateway

	if event.RequestID != "req-1" || event.ConsumerName != "alpha" || event.StatusCode != http.StatusOK {
		t.Fatalf("unexpected decision metadata: %+v", event)
	}
	if event.ModelRequested != "jev-1.14.0" || event.ModelUsed != "jev-1.14.0" || event.Vendor != "typesafe" {
		t.Fatalf("unexpected call metadata: %+v", event)
	}
	if event.Usage == nil || event.Usage.TotalTokens != 17 || len(event.Attempts) != 1 || event.Attempts[0].StatusCode != http.StatusOK {
		t.Fatalf("unexpected usage or attempt: %+v", event)
	}
	payload := decisionPayload(t, event)
	if got := string(payload["decision_request"]); got != request {
		t.Errorf("decision_request = %s, want %s", got, request)
	}
	if got := string(payload["decision_response"]); got != response {
		t.Errorf("decision_response = %s, want %s", got, response)
	}
	if payload["request"] != nil || payload["response"] != nil {
		t.Errorf("decision event contains chat payload fields: %v", payload)
	}
}

func TestFromDecision_FailurePayloadFollowsAttempt(t *testing.T) {
	request := json.RawMessage(`{"state":"private"}`)
	audit := &telemetry.AuditEvent{EventCommon: telemetry.EventCommon{StatusCode: http.StatusForbidden}}
	input := DecisionBuildInput{Audit: audit, Call: &telemetry.CallEvent{}, Request: request}
	if got, ok := FromDecision(input); ok || got != nil {
		t.Fatalf("FromDecision returned an event for a denied request: %+v", got)
	}

	audit.StatusCode = http.StatusBadGateway
	audit.Kind = llmtypes.KindUpstream
	input.Call.Attempts = []llmtypes.Attempt{{Vendor: "typesafe", StatusCode: http.StatusBadGateway}}
	event, ok := FromDecision(input)
	if !ok || event.StatusCode != http.StatusBadGateway || event.ErrorKind != llmtypes.KindUpstream {
		t.Fatalf("unexpected decision failure event: %+v", event)
	}
	payload := decisionPayload(t, event)
	if string(payload["decision_request"]) != string(request) || payload["decision_response"] != nil {
		t.Errorf("decision failure payload contains unexpected fields: %v", payload)
	}
}

func decisionPayload(t *testing.T, event *Event) map[string]json.RawMessage {
	t.Helper()
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}
