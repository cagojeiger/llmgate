package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"llmgate/internal/provider"
)

func newCapturingLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), buf
}

func TestLogRecorder_RecordSuccess(t *testing.T) {
	log, buf := newCapturingLogger()
	r := NewLogRecorder(log)

	rec := &Record{
		Timestamp:     time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		RequestID:     "req-1",
		Method:        "chat.completions",
		ModelRequested: "deepseek-v4-flash",
		StatusCode:    200,
		DurationMS:    234,
		RequestBytes:  100,
		ResponseBytes: 500,
		Usage: &provider.Usage{
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
	if out["request_id"] != "req-1" || out["model_requested"] != "deepseek-v4-flash" {
		t.Errorf("missing core fields: %+v", out)
	}
	if out["prompt_tokens"].(float64) != 10 {
		t.Errorf("prompt_tokens = %v, want 10", out["prompt_tokens"])
	}
	if out["vendor_cost"] != "0.001" {
		t.Errorf("vendor_cost = %v, want 0.001", out["vendor_cost"])
	}
	if _, ok := out["error_kind"]; ok {
		t.Errorf("error_kind should be omitted on success: %+v", out)
	}
}

func TestLogRecorder_RecordError(t *testing.T) {
	log, buf := newCapturingLogger()
	r := NewLogRecorder(log)

	r.Record(context.Background(), &Record{
		Timestamp:      time.Now(),
		RequestID:      "req-2",
		Method:         "chat.completions",
		ModelRequested: "kimi-k2.6",
		StatusCode:     429,
		ErrorKind:      provider.KindRateLimit,
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

func TestLogRecorder_RecordNil(t *testing.T) {
	log, buf := newCapturingLogger()
	r := NewLogRecorder(log)
	r.Record(context.Background(), nil)
	if buf.Len() != 0 {
		t.Errorf("nil record should emit nothing, got %s", buf.String())
	}
}

func TestLogRecorder_FallbackFields(t *testing.T) {
	log, buf := newCapturingLogger()
	r := NewLogRecorder(log)

	r.Record(context.Background(), &Record{
		Timestamp:      time.Now(),
		RequestID:      "req-3",
		Method:         "chat.completions",
		ModelRequested: "coder",
		Vendor:         "opencode",
		ModelUsed:      "deepseek-v4-flash",
		StatusCode:     200,
		Usage:          &provider.Usage{PromptTokens: 5, CompletionTokens: 7, TotalTokens: 12},
		Attempts: []provider.Attempt{
			{Vendor: "opencode", Model: "deepseek-v4-pro", DurationMS: 80, ErrorKind: provider.KindRateLimit, StatusCode: 429},
			{Vendor: "opencode", Model: "deepseek-v4-flash", DurationMS: 200, StatusCode: 200, Usage: &provider.Usage{PromptTokens: 5, CompletionTokens: 7, TotalTokens: 12}},
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

func TestLogRecorder_OmitsAttemptsWhenSingle(t *testing.T) {
	log, buf := newCapturingLogger()
	r := NewLogRecorder(log)

	r.Record(context.Background(), &Record{
		Timestamp:      time.Now(),
		RequestID:      "req-4",
		Method:         "chat.completions",
		ModelRequested: "deepseek-v4-flash",
		Vendor:         "opencode",
		ModelUsed:      "deepseek-v4-flash",
		StatusCode:     200,
		Attempts: []provider.Attempt{
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

func TestComposite(t *testing.T) {
	a, b := &captureRecorder{}, &captureRecorder{}
	c := Composite{a, b}
	c.Record(context.Background(), &Record{ModelRequested: "m"})

	if len(a.calls) != 1 || len(b.calls) != 1 {
		t.Errorf("each recorder should have 1 call, got a=%d b=%d", len(a.calls), len(b.calls))
	}
}

func TestComposite_NilElement(t *testing.T) {
	a := &captureRecorder{}
	c := Composite{nil, a, nil}
	c.Record(context.Background(), &Record{ModelRequested: "m"})
	if len(a.calls) != 1 {
		t.Errorf("non-nil element should still receive: %d", len(a.calls))
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close with all-nil-or-no-error should be nil, got %v", err)
	}
}

func TestNop(t *testing.T) {
	var n Nop
	n.Record(context.Background(), &Record{ModelRequested: "x"})
	if err := n.Close(); err != nil {
		t.Errorf("Nop.Close = %v, want nil", err)
	}
}
