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

func TestSlogRecorder_RecordSuccess(t *testing.T) {
	log, buf := newCapturingLogger()
	r := NewSlogRecorder(log)

	rec := &Record{
		Timestamp:      time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		RequestID:      "req-1",
		Operation:      "chat.completions",
		ModelRequested: "deepseek-v4-flash",
		StatusCode:     200,
		DurationMS:     234,
		RequestBytes:   100,
		ResponseBytes:  500,
		Usage: &llmtypes.Usage{
			PromptTokens:     10,
			CompletionTokens: 20,
			TotalTokens:      30,
		},
		VendorCost: "0.001",
	}
	r.Record(context.Background(), rec)

	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal log line: %v", err)
	}
	if out["msg"] != "audit" {
		t.Errorf("msg = %v, want audit", out["msg"])
	}
	if out["operation"] != "chat.completions" {
		t.Errorf("operation = %v, want chat.completions", out["operation"])
	}
	if out["request_id"] != "req-1" || out["model_requested"] != "deepseek-v4-flash" {
		t.Errorf("missing core fields: %+v", out)
	}
	if out["prompt_tokens"].(float64) != 10 {
		t.Errorf("prompt_tokens = %v, want 10", out["prompt_tokens"])
	}
	if out["vendor_cost"] != "0.001" {
		t.Errorf("vendor_cost = %v, want 0.001", out["vendor_cost"])
	}
	if out["duration_ms"].(float64) != 234 {
		t.Errorf("duration_ms = %v, want 234", out["duration_ms"])
	}
	if _, ok := out["error_kind"]; ok {
		t.Errorf("error_kind should be omitted on success: %+v", out)
	}
}

func TestSlogRecorder_RecordAuthFields(t *testing.T) {
	// Caller-identity attrs land on the success line so post-processing
	// can answer "who called?" from the audit stream alone.
	log, buf := newCapturingLogger()
	r := NewSlogRecorder(log)

	r.Record(context.Background(), &Record{
		Timestamp:      time.Now(),
		RequestID:      "req-auth-ok",
		Operation:      "chat.completions",
		ModelRequested: "deepseek-v4-flash",
		ConsumerName:   "alpha",
		ConsumerKeyID:  "01234567",
		StatusCode:     200,
	})

	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["consumer_name"] != "alpha" {
		t.Errorf("consumer_name = %v, want alpha", out["consumer_name"])
	}
	if out["consumer_key_id"] != "01234567" {
		t.Errorf("consumer_key_id = %v, want 01234567", out["consumer_key_id"])
	}
	if _, ok := out["auth_error"]; ok {
		t.Errorf("auth_error must be omitted on success: %+v", out)
	}
}

func TestSlogRecorder_RecordAuthFailure(t *testing.T) {
	// The audit-always property hinges on this: a 401 must still leave a
	// recognizable line in the audit stream, with auth_error pinning the
	// failure mode and ConsumerName/KeyID empty (kept off the line).
	log, buf := newCapturingLogger()
	r := NewSlogRecorder(log)

	r.Record(context.Background(), &Record{
		Timestamp:      time.Now(),
		RequestID:      "req-auth-bad",
		Operation:      "chat.completions",
		ModelRequested: "",
		AuthError:      AuthErrorUnknown,
		Kind:           llmtypes.KindAuth,
		StatusCode:     401,
	})

	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["auth_error"] != "unknown" {
		t.Errorf("auth_error = %v, want unknown", out["auth_error"])
	}
	if _, ok := out["consumer_name"]; ok {
		t.Errorf("consumer_name must be omitted on auth failure: %+v", out)
	}
	if _, ok := out["consumer_key_id"]; ok {
		t.Errorf("consumer_key_id must be omitted on auth failure: %+v", out)
	}
}

func TestSlogRecorder_RecordError(t *testing.T) {
	log, buf := newCapturingLogger()
	r := NewSlogRecorder(log)

	r.Record(context.Background(), &Record{
		Timestamp:      time.Now(),
		RequestID:      "req-2",
		Operation:      "chat.completions",
		ModelRequested: "kimi-k2.6",
		StatusCode:     429,
		Kind:           llmtypes.KindRateLimit,
		DurationMS:     50,
	})

	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["error_kind"] != "rate_limit" {
		t.Errorf("error_kind = %v, want rate_limit", out["error_kind"])
	}
	if _, ok := out["prompt_tokens"]; ok {
		t.Errorf("prompt_tokens should be omitted when Usage is nil: %+v", out)
	}
}

func TestSlogRecorder_RecordNil(t *testing.T) {
	log, buf := newCapturingLogger()
	r := NewSlogRecorder(log)
	r.Record(context.Background(), nil)
	if buf.Len() != 0 {
		t.Errorf("nil record should emit nothing, got %s", buf.String())
	}
}

func TestSlogRecorder_FallbackFields(t *testing.T) {
	log, buf := newCapturingLogger()
	r := NewSlogRecorder(log)

	r.Record(context.Background(), &Record{
		Timestamp:      time.Now(),
		RequestID:      "req-3",
		Operation:      "chat.completions",
		ModelRequested: "coder",
		Vendor:         "opencode",
		ModelUsed:      "deepseek-v4-flash",
		StatusCode:     200,
		Usage:          &llmtypes.Usage{PromptTokens: 5, CompletionTokens: 7, TotalTokens: 12},
		Attempts: []llmtypes.Attempt{
			{Vendor: "opencode", Model: "deepseek-v4-pro", DurationMS: 80, Kind: llmtypes.KindRateLimit, StatusCode: 429},
			{Vendor: "opencode", Model: "deepseek-v4-flash", DurationMS: 200, StatusCode: 200, Usage: &llmtypes.Usage{PromptTokens: 5, CompletionTokens: 7, TotalTokens: 12}},
		},
	})

	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["model_requested"] != "coder" {
		t.Errorf("model_requested = %v, want coder", out["model_requested"])
	}
	if out["model_used"] != "deepseek-v4-flash" {
		t.Errorf("model_used = %v, want deepseek-v4-flash", out["model_used"])
	}
	if out["vendor"] != "opencode" {
		t.Errorf("vendor = %v, want opencode", out["vendor"])
	}
	atts, ok := out["attempts"].([]any)
	if !ok || len(atts) != 2 {
		t.Fatalf("attempts = %v, want 2-item slice", out["attempts"])
	}
}

func TestSlogRecorder_OmitsAttemptsWhenSingle(t *testing.T) {
	log, buf := newCapturingLogger()
	r := NewSlogRecorder(log)

	r.Record(context.Background(), &Record{
		Timestamp:      time.Now(),
		RequestID:      "req-4",
		Operation:      "chat.completions",
		ModelRequested: "deepseek-v4-flash",
		Vendor:         "opencode",
		ModelUsed:      "deepseek-v4-flash",
		StatusCode:     200,
		Attempts: []llmtypes.Attempt{
			{Vendor: "opencode", Model: "deepseek-v4-flash", StatusCode: 200},
		},
	})

	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := out["attempts"]; ok {
		t.Errorf("attempts must be omitted for single-attempt records: %+v", out)
	}
	// model_used == model_requested, so model_used should be omitted to keep
	// non-fallback log lines compact.
	if _, ok := out["model_used"]; ok {
		t.Errorf("model_used must be omitted when same as requested: %+v", out)
	}
}

type captureRecorder struct {
	calls []*Record
	err   error
}

func (c *captureRecorder) Record(_ context.Context, r *Record) {
	c.calls = append(c.calls, r)
}

func (c *captureRecorder) Close() error { return c.err }

func TestRecorders(t *testing.T) {
	a, b := &captureRecorder{}, &captureRecorder{}
	c := Recorders{a, b}
	c.Record(context.Background(), &Record{ModelRequested: "m"})

	if len(a.calls) != 1 || len(b.calls) != 1 {
		t.Errorf("each recorder should have 1 call, got a=%d b=%d", len(a.calls), len(b.calls))
	}
}

func TestRecorders_NilElement(t *testing.T) {
	a := &captureRecorder{}
	c := Recorders{nil, a, nil}
	c.Record(context.Background(), &Record{ModelRequested: "m"})
	if len(a.calls) != 1 {
		t.Errorf("non-nil element should still receive: %d", len(a.calls))
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close with all-nil-or-no-error should be nil, got %v", err)
	}
}

func TestRecorders_CloseReturnsFirstErrButStillRunsRest(t *testing.T) {
	first := errors.New("first-failed")
	second := errors.New("second-failed")
	a := &captureRecorder{err: first}
	b := &captureRecorder{err: second}
	c := &captureRecorder{}
	rs := Recorders{a, b, c}

	got := rs.Close()
	if !errors.Is(got, first) {
		t.Errorf("Close = %v, want first-failed (first non-nil)", got)
	}
	// All sinks were given a chance to close — captureRecorder records
	// nothing for Close, but the contract is "best-effort, return first
	// err". This test pins that "first" semantics.
}

func TestNop(t *testing.T) {
	var n Nop
	n.Record(context.Background(), &Record{ModelRequested: "x"})
	if err := n.Close(); err != nil {
		t.Errorf("Nop.Close = %v, want nil", err)
	}
}
