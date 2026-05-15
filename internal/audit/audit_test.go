package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"llmgate/internal/llmtypes"
)

func newCapturingLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), buf
}

func TestSlogRecorder_RecordAudit(t *testing.T) {
	log, buf := newCapturingLogger()
	r := NewSlogRecorder(log)

	r.RecordAudit(context.Background(), &Record{
		EventCommon: EventCommon{
			Timestamp:     time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			RequestID:     "req-1",
			Operation:     "chat.completions",
			ConsumerName:  "alpha",
			ConsumerKeyID: "01234567",
			StatusCode:    200,
			DurationMS:    234,
		},
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
	if _, ok := out["model_requested"]; ok {
		t.Errorf("audit record must not include call fields: %+v", out)
	}
}

func TestSlogRecorder_RecordAuthFailure(t *testing.T) {
	log, buf := newCapturingLogger()
	r := NewSlogRecorder(log)

	r.RecordAudit(context.Background(), &Record{
		EventCommon: EventCommon{
			Timestamp:  time.Now(),
			RequestID:  "req-auth-bad",
			Operation:  "chat.completions",
			Kind:       llmtypes.KindAuth,
			StatusCode: 401,
		},
		AuthError: AuthErrorUnknown,
	})

	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["auth_error"] != "unknown" || out["error_kind"] != "auth" {
		t.Errorf("auth fields = %+v, want auth_error=unknown error_kind=auth", out)
	}
	if _, ok := out["consumer_name"]; ok {
		t.Errorf("consumer_name must be omitted on auth failure: %+v", out)
	}
}

func TestSlogCallRecorder_RecordCall(t *testing.T) {
	log, buf := newCapturingLogger()
	r := NewSlogCallRecorder(log)

	r.RecordCall(context.Background(), &CallRecord{
		EventCommon: EventCommon{
			Timestamp:     time.Now(),
			RequestID:     "req-call",
			Operation:     "chat.completions",
			ConsumerName:  "alpha",
			ConsumerKeyID: "01234567",
			StatusCode:    200,
			DurationMS:    50,
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
	atts, ok := out["attempts"].([]any)
	if !ok || len(atts) != 2 {
		t.Fatalf("attempts = %v, want 2-item slice", out["attempts"])
	}
}

func TestSlogCallRecorder_OmitsAttemptsWhenSingle(t *testing.T) {
	log, buf := newCapturingLogger()
	r := NewSlogCallRecorder(log)

	r.RecordCall(context.Background(), &CallRecord{
		EventCommon:    EventCommon{Timestamp: time.Now(), RequestID: "req-4", Operation: "chat.completions", StatusCode: 200},
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

func TestSlogRecorders_RecordNil(t *testing.T) {
	log, buf := newCapturingLogger()
	NewSlogRecorder(log).RecordAudit(context.Background(), nil)
	NewSlogCallRecorder(log).RecordCall(context.Background(), nil)
	if buf.Len() != 0 {
		t.Errorf("nil record should emit nothing, got %s", buf.String())
	}
}

type captureRecorder struct {
	calls []*Record
	err   error
}

func (c *captureRecorder) RecordAudit(_ context.Context, r *Record) {
	c.calls = append(c.calls, r)
}

func (c *captureRecorder) Close() error { return c.err }

type captureCallRecorder struct {
	calls []*CallRecord
	err   error
}

func (c *captureCallRecorder) RecordCall(_ context.Context, r *CallRecord) {
	c.calls = append(c.calls, r)
}

func (c *captureCallRecorder) Close() error { return c.err }

func TestRecorders(t *testing.T) {
	a, b := &captureRecorder{}, &captureRecorder{}
	c := Recorders{a, b}
	c.RecordAudit(context.Background(), &Record{})

	if len(a.calls) != 1 || len(b.calls) != 1 {
		t.Errorf("each recorder should have 1 call, got a=%d b=%d", len(a.calls), len(b.calls))
	}
}

func TestCallRecorders(t *testing.T) {
	a, b := &captureCallRecorder{}, &captureCallRecorder{}
	c := CallRecorders{a, b}
	c.RecordCall(context.Background(), &CallRecord{})

	if len(a.calls) != 1 || len(b.calls) != 1 {
		t.Errorf("each call recorder should have 1 call, got a=%d b=%d", len(a.calls), len(b.calls))
	}
}

func TestRecorders_CloseReturnsFirstErrButStillRunsRest(t *testing.T) {
	first := errors.New("first-failed")
	second := errors.New("second-failed")
	rs := Recorders{&captureRecorder{err: first}, &captureRecorder{err: second}, &captureRecorder{}}

	got := rs.Close()
	if !errors.Is(got, first) {
		t.Errorf("Close = %v, want first-failed", got)
	}
}

func TestNop(t *testing.T) {
	var n Nop
	n.RecordAudit(context.Background(), &Record{})
	if err := n.Close(); err != nil {
		t.Errorf("Nop.Close = %v, want nil", err)
	}
}
