package telemetry

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"llmgate/internal/llmtypes"
)

func TestSlogSink_RecordAudit(t *testing.T) {
	log, buf := newCapturingLogger()
	sink := NewSlogSink(log, log)

	sink.Emit(context.Background(), &AuditEvent{
		EventCommon: EventCommon{
			Timestamp:      time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			RequestID:      "req-1",
			ServiceName:    "llmgate",
			ServiceVersion: "v0.1.6",
			Environment:    "prod",
			Operation:      "chat.completions",
			ConsumerName:   "alpha",
			ConsumerKeyID:  "01234567",
			StatusCode:     200,
			DurationMS:     234,
		},
		AuthResult:   AuthResultSuccess,
		PolicyResult: PolicyResultAllowed,
		ResourceType: "llm_model",
		ResourceID:   "coder",
	})

	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal log line: %v", err)
	}
	if out["msg"] != "audit" || out["event_type"] != "audit" {
		t.Fatalf("audit event fields missing: %+v", out)
	}
	if out["schema_version"].(float64) != 1 {
		t.Errorf("schema_version = %v, want 1", out["schema_version"])
	}
	if out["request_id"] != "req-1" || out["consumer_name"] != "alpha" {
		t.Errorf("missing identity fields: %+v", out)
	}
	if out["service_name"] != "llmgate" || out["service_version"] != "v0.1.6" || out["environment"] != "prod" {
		t.Errorf("missing envelope fields: %+v", out)
	}
	if out["auth_result"] != "success" || out["policy_result"] != "allowed" {
		t.Errorf("missing audit decision fields: %+v", out)
	}
	if out["resource_type"] != "llm_model" || out["resource_id"] != "coder" {
		t.Errorf("missing resource fields: %+v", out)
	}
	if _, ok := out["model_requested"]; ok {
		t.Errorf("audit record must not include call fields: %+v", out)
	}
}

func TestSlogSink_RecordAuthFailure(t *testing.T) {
	log, buf := newCapturingLogger()
	sink := NewSlogSink(log, log)

	sink.Emit(context.Background(), &AuditEvent{
		EventCommon: EventCommon{
			Timestamp:  time.Now(),
			RequestID:  "req-auth-bad",
			Operation:  "chat.completions",
			Kind:       llmtypes.KindAuth,
			StatusCode: 401,
		},
		AuthResult:   AuthResultFailure,
		AuthError:    AuthErrorUnknown,
		PolicyResult: PolicyResultDenied,
		DenyReason:   DenyReasonAuth,
	})

	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["auth_error"] != "unknown" || out["error_kind"] != "auth" {
		t.Errorf("auth fields = %+v, want auth_error=unknown error_kind=auth", out)
	}
	if out["auth_result"] != "failure" || out["policy_result"] != "denied" || out["deny_reason"] != "auth" {
		t.Errorf("auth decision fields = %+v, want failure/denied/auth", out)
	}
	if _, ok := out["consumer_name"]; ok {
		t.Errorf("consumer_name must be omitted on auth failure: %+v", out)
	}
}

func TestSlogSink_RecordCall(t *testing.T) {
	log, buf := newCapturingLogger()
	sink := NewSlogSink(log, log)

	sink.Emit(context.Background(), &CallEvent{
		EventCommon: EventCommon{
			Timestamp:      time.Now(),
			RequestID:      "req-call",
			ServiceName:    "llmgate",
			ServiceVersion: "v0.1.6",
			Environment:    "prod",
			Operation:      "chat.completions",
			ConsumerName:   "alpha",
			ConsumerKeyID:  "01234567",
			StatusCode:     200,
			DurationMS:     50,
		},
		ModelRequested: "coder",
		ModelUsed:      "deepseek-v4-flash",
		Vendor:         "opencode",
		RequestBytes:   100,
		ResponseBytes:  500,
		Usage:          &llmtypes.Usage{PromptTokens: 5, CompletionTokens: 7, TotalTokens: 12},
		VendorCost:     "0.001",
		Attempts: []llmtypes.Attempt{
			{Vendor: "opencode", Model: "deepseek-v4-pro", DurationMS: 80, Kind: llmtypes.KindRateLimit, StatusCode: 429},
			{Vendor: "opencode", Model: "deepseek-v4-flash", DurationMS: 200, StatusCode: 200},
		},
	})

	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["msg"] != "call" || out["event_type"] != "call" {
		t.Fatalf("call event fields missing: %+v", out)
	}
	if out["model_requested"] != "coder" || out["model_used"] != "deepseek-v4-flash" {
		t.Errorf("model fields = %+v", out)
	}
	if out["prompt_tokens"].(float64) != 5 || out["total_tokens"].(float64) != 12 {
		t.Errorf("usage fields = %+v", out)
	}
	if out["attempts_count"].(float64) != 2 {
		t.Errorf("attempts_count = %v, want 2", out["attempts_count"])
	}
	if out["final_attempt_vendor"] != "opencode" || out["final_attempt_model"] != "deepseek-v4-flash" {
		t.Errorf("final attempt fields = %+v", out)
	}
	atts, ok := out["attempts"].([]any)
	if !ok || len(atts) != 2 {
		t.Fatalf("attempts = %v, want 2-item slice", out["attempts"])
	}
}

func TestSlogSink_DoNotLeakSensitiveMaterial(t *testing.T) {
	log, buf := newCapturingLogger()
	sink := NewSlogSink(log, log)

	sink.Emit(context.Background(), &AuditEvent{
		EventCommon: EventCommon{
			Timestamp:     time.Now(),
			RequestID:     "req-redaction",
			Operation:     "chat.completions",
			ConsumerName:  "alpha",
			ConsumerKeyID: "01234567",
			StatusCode:    200,
		},
		AuthResult: AuthResultSuccess,
	})
	sink.Emit(context.Background(), &CallEvent{
		EventCommon: EventCommon{
			Timestamp:     time.Now(),
			RequestID:     "req-redaction",
			Operation:     "chat.completions",
			ConsumerName:  "alpha",
			ConsumerKeyID: "01234567",
			StatusCode:    200,
		},
		ModelRequested: "coder",
		RequestBytes:   256,
		ResponseBytes:  512,
		Attempts:       []llmtypes.Attempt{{Vendor: "opencode", Model: "deepseek-v4-flash", StatusCode: 200}},
	})

	logged := buf.String()
	for _, forbidden := range []string{
		"Authorization",
		"Bearer ",
		"sk-test-secret",
		"hello prompt body",
		"assistant response body",
	} {
		if strings.Contains(logged, forbidden) {
			t.Fatalf("log leaked %q: %s", forbidden, logged)
		}
	}
}

func TestSlogSink_OmitsAttemptsWhenSingle(t *testing.T) {
	log, buf := newCapturingLogger()
	sink := NewSlogSink(log, log)

	sink.Emit(context.Background(), &CallEvent{
		EventCommon: EventCommon{
			Timestamp:  time.Now(),
			RequestID:  "req-4",
			Operation:  "chat.completions",
			StatusCode: 200,
		},
		ModelRequested: "deepseek-v4-flash",
		Vendor:         "opencode",
		ModelUsed:      "deepseek-v4-flash",
		Attempts:       []llmtypes.Attempt{{Vendor: "opencode", Model: "deepseek-v4-flash", StatusCode: 200}},
	})

	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := out["attempts"]; ok {
		t.Errorf("attempts must be omitted for single-attempt records: %+v", out)
	}
	if _, ok := out["model_used"]; ok {
		t.Errorf("model_used must be omitted when same as requested: %+v", out)
	}
}

func TestSlogSink_RecordNil(t *testing.T) {
	log, buf := newCapturingLogger()
	sink := NewSlogSink(log, log)
	sink.Emit(context.Background(), (*AuditEvent)(nil))
	sink.Emit(context.Background(), (*CallEvent)(nil))
	if buf.Len() != 0 {
		t.Errorf("nil record should emit nothing, got %s", buf.String())
	}
}
