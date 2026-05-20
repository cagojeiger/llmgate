package llmresult

import (
	"net/http"
	"testing"
	"time"

	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/telemetry"
)

func TestFromTelemetry_BuildsFinalizedResultEvent(t *testing.T) {
	ts := time.Unix(1700000000, 0)
	audit := &telemetry.AuditEvent{
		EventCommon: telemetry.EventCommon{
			Timestamp:      ts,
			RequestID:      "req-1",
			ServiceName:    "llmgate",
			ServiceVersion: "dev",
			Environment:    "test",
			Operation:      "chat.completions",
			ConsumerName:   "alpha",
			ConsumerKeyID:  "abcd1234",
			StatusCode:     http.StatusOK,
			DurationMS:     42,
		},
	}
	call := &telemetry.CallEvent{
		EventCommon:    audit.EventCommon,
		ModelRequested: "smart",
		ModelUsed:      "deepseek-v4-flash",
		Vendor:         "opencode",
		RequestBytes:   123,
		ResponseBytes:  456,
		Usage:          &llmtypes.Usage{PromptTokens: 5, CompletionTokens: 7, TotalTokens: 12},
		VendorCost:     `"0.001"`,
		Attempts: []llmtypes.Attempt{
			{Vendor: "opencode", Model: "deepseek-v4-flash", StatusCode: http.StatusOK, StartedAt: ts},
		},
	}
	req := &llmtypes.Request{
		Model:    "smart",
		Messages: []llmtypes.Message{{Role: "user", Content: "hello"}},
	}
	resp := &llmtypes.Response{
		Model: "deepseek-v4-flash",
		Choices: []llmtypes.Choice{{
			Index:        0,
			Message:      llmtypes.Message{Role: "assistant", Content: "world"},
			FinishReason: "stop",
		}},
	}

	got, ok := FromTelemetry(BuildInput{Audit: audit, Call: call, Request: req, Response: resp})
	if !ok {
		t.Fatal("FromTelemetry ok = false, want true")
	}
	if got.SchemaVersion != SchemaVersion || got.EventType != EventType {
		t.Fatalf("schema/event = %d/%q, want %d/%q", got.SchemaVersion, got.EventType, SchemaVersion, EventType)
	}
	if got.RequestID != "req-1" || got.ConsumerName != "alpha" {
		t.Fatalf("common fields = request_id:%q consumer:%q", got.RequestID, got.ConsumerName)
	}
	if got.Request == req || got.Response == resp {
		t.Fatal("request/response were not cloned")
	}
	if got.Request.Messages[0].Content != "hello" {
		t.Errorf("request content = %q, want hello", got.Request.Messages[0].Content)
	}
	if got.Response.Choices[0].Message.Content != "world" {
		t.Errorf("response content = %q, want world", got.Response.Choices[0].Message.Content)
	}
	if got.ModelRequested != "smart" || got.ModelUsed != "deepseek-v4-flash" || got.Vendor != "opencode" {
		t.Errorf("routing fields = %+v", got)
	}
	if got.Usage.TotalTokens != 12 {
		t.Errorf("usage total = %d, want 12", got.Usage.TotalTokens)
	}
	if len(got.Attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(got.Attempts))
	}
}

func TestFromTelemetry_SkipsWhenCallWasNotAttempted(t *testing.T) {
	audit := &telemetry.AuditEvent{EventCommon: telemetry.EventCommon{RequestID: "req-1"}}

	if got, ok := FromTelemetry(BuildInput{Audit: audit, Call: &telemetry.CallEvent{}}); ok || got != nil {
		t.Fatalf("FromTelemetry = (%v, %v), want nil,false", got, ok)
	}
}

func TestFromTelemetry_ClonesRequestAndResponse(t *testing.T) {
	audit := &telemetry.AuditEvent{EventCommon: telemetry.EventCommon{RequestID: "req-1"}}
	call := &telemetry.CallEvent{
		Usage:    &llmtypes.Usage{TotalTokens: 1},
		Attempts: []llmtypes.Attempt{{Vendor: "v", Usage: &llmtypes.Usage{TotalTokens: 1}}},
	}
	req := &llmtypes.Request{Messages: []llmtypes.Message{{Role: "user", Content: "before"}}}
	resp := &llmtypes.Response{Choices: []llmtypes.Choice{{Message: llmtypes.Message{Content: "before"}}}}

	got, ok := FromTelemetry(BuildInput{Audit: audit, Call: call, Request: req, Response: resp})
	if !ok {
		t.Fatal("FromTelemetry ok = false, want true")
	}
	req.Messages[0].Content = "after"
	resp.Choices[0].Message.Content = "after"
	call.Usage.TotalTokens = 2
	call.Attempts[0].Usage.TotalTokens = 2

	if got.Request.Messages[0].Content != "before" {
		t.Errorf("cloned request content = %q, want before", got.Request.Messages[0].Content)
	}
	if got.Response.Choices[0].Message.Content != "before" {
		t.Errorf("cloned response content = %q, want before", got.Response.Choices[0].Message.Content)
	}
	if got.Usage.TotalTokens != 1 {
		t.Errorf("cloned usage total = %d, want 1", got.Usage.TotalTokens)
	}
	if got.Attempts[0].Usage.TotalTokens != 1 {
		t.Errorf("cloned attempt usage total = %d, want 1", got.Attempts[0].Usage.TotalTokens)
	}
}
