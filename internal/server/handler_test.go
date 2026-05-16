package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"llmgate/internal/llmrouter"
	"llmgate/internal/llmtypes"
	"llmgate/internal/telemetry"
)

func TestHandler_SingleAttempt_RecordPopulated(t *testing.T) {
	rec, recorder := newCaptureAuditSink()
	callRec, callSink := newCaptureCallSink()
	r := &fakeService{
		vendor: "opencode",
		buildResult: func(req *llmtypes.Request) *llmrouter.RouteResult {
			return &llmrouter.RouteResult{
				Response: &llmtypes.Response{
					Model:   req.Model,
					Choices: []llmtypes.Choice{{Index: 0, Message: llmtypes.Message{Role: "assistant", Content: "ok"}}},
				},
				Vendor:    "opencode",
				ModelUsed: req.Model,
				Attempts: []llmtypes.Attempt{
					{Vendor: "opencode", Model: req.Model, StatusCode: 200, StartedAt: time.Now()},
				},
			}
		},
	}
	h := newTestHandler(r, recorder, callSink, HandlerConfig{})

	body := `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	gotAudit := rec.last(t)
	if gotAudit.StatusCode != http.StatusOK {
		t.Errorf("audit StatusCode = %d, want 200", gotAudit.StatusCode)
	}
	if gotAudit.AuthResult != telemetry.AuthResultSuccess || gotAudit.PolicyResult != telemetry.PolicyResultAllowed {
		t.Errorf("audit decisions = auth:%q policy:%q, want success/allowed", gotAudit.AuthResult, gotAudit.PolicyResult)
	}
	if gotAudit.ResourceType != "llm_model" || gotAudit.ResourceID != "deepseek-v4-flash" {
		t.Errorf("audit resource = %q/%q, want llm_model/deepseek-v4-flash", gotAudit.ResourceType, gotAudit.ResourceID)
	}
	got := callRec.last(t)
	if got.ModelRequested != "deepseek-v4-flash" {
		t.Errorf("ModelRequested = %q, want deepseek-v4-flash", got.ModelRequested)
	}
	if got.Vendor != "opencode" || got.ModelUsed != "deepseek-v4-flash" {
		t.Errorf("Vendor/ModelUsed = %q/%q, want opencode/deepseek-v4-flash", got.Vendor, got.ModelUsed)
	}
	if len(got.Attempts) != 1 {
		t.Fatalf("len(Attempts) = %d, want 1", len(got.Attempts))
	}
	if got.Attempts[0].Vendor != "opencode" || got.Attempts[0].Model != "deepseek-v4-flash" {
		t.Errorf("attempt = %+v, want opencode/deepseek-v4-flash", got.Attempts[0])
	}
}

func TestHandler_FallbackChain_AttemptsRecorded(t *testing.T) {
	_, recorder := newCaptureAuditSink()
	callRec, callSink := newCaptureCallSink()
	r := &fakeService{
		vendor: "opencode",
		buildResult: func(req *llmtypes.Request) *llmrouter.RouteResult {
			return &llmrouter.RouteResult{
				Response: &llmtypes.Response{
					Model:   "deepseek-v4-flash",
					Choices: []llmtypes.Choice{{Index: 0, Message: llmtypes.Message{Role: "assistant", Content: "ok"}}},
				},
				Vendor:    "opencode",
				ModelUsed: "deepseek-v4-flash",
				Attempts: []llmtypes.Attempt{
					{
						Vendor:     "opencode",
						Model:      "deepseek-v4-pro",
						Kind:       llmtypes.KindRateLimit,
						StatusCode: 429,
						StartedAt:  time.Now(),
					},
					{Vendor: "opencode", Model: "deepseek-v4-flash", StatusCode: 200, StartedAt: time.Now()},
				},
			}
		},
	}
	h := newTestHandler(r, recorder, callSink, HandlerConfig{})

	body := `{"model":"coder","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	got := callRec.last(t)
	if got.ModelRequested != "coder" {
		t.Errorf("ModelRequested = %q, want coder (alias)", got.ModelRequested)
	}
	if got.ModelUsed != "deepseek-v4-flash" {
		t.Errorf("ModelUsed = %q, want deepseek-v4-flash (last attempt)", got.ModelUsed)
	}
	if len(got.Attempts) != 2 {
		t.Fatalf("len(Attempts) = %d, want 2", len(got.Attempts))
	}
	if got.Attempts[0].Kind != llmtypes.KindRateLimit {
		t.Errorf("attempts[0].Kind = %q, want rate_limit", got.Attempts[0].Kind)
	}
}

func TestAdoptError_ProviderErrorMapsKindAndStatus(t *testing.T) {
	cases := []struct {
		name       string
		kind       llmtypes.ErrorKind
		wantStatus int
	}{
		{"auth", llmtypes.KindAuth, http.StatusUnauthorized},
		{"forbidden", llmtypes.KindForbidden, http.StatusForbidden},
		{"rate_limit", llmtypes.KindRateLimit, http.StatusTooManyRequests},
		{"bad_request", llmtypes.KindBadRequest, http.StatusBadRequest},
		{"context_length", llmtypes.KindContextLength, http.StatusBadRequest},
		{"upstream", llmtypes.KindUpstream, http.StatusBadGateway},
		{"timeout", llmtypes.KindTimeout, http.StatusBadGateway},
		{"unknown", llmtypes.KindUnknown, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &telemetry.AuditEvent{}
			adoptError(rec, &llmtypes.Error{Kind: tc.kind, Message: "x"})
			if rec.Kind != tc.kind {
				t.Errorf("Kind = %q, want %q", rec.Kind, tc.kind)
			}
			if rec.StatusCode != tc.wantStatus {
				t.Errorf("StatusCode = %d, want %d", rec.StatusCode, tc.wantStatus)
			}
		})
	}
}

func TestHandler_AllowedAliasesRejectBeforeService(t *testing.T) {
	rec, recorder := newCaptureAuditSink()
	callRec, callSink := newCaptureCallSink()
	serviceCalled := false
	r := &fakeService{
		buildResult: func(req *llmtypes.Request) *llmrouter.RouteResult {
			serviceCalled = true
			return &llmrouter.RouteResult{
				Response: &llmtypes.Response{Model: req.Model},
				Vendor:   "opencode",
			}
		},
	}
	h := newTestHandler(r, recorder, callSink, HandlerConfig{})

	body := `{"model":"smart","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), consumerCtxKey{}, &ConsumerInfo{
		Name:           "alpha",
		KeyID:          "12345678",
		AllowedAliases: []string{"cheap"},
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", w.Code, w.Body.String())
	}
	if serviceCalled {
		t.Fatal("service called for disallowed model")
	}
	got := rec.last(t)
	if got.ConsumerName != "alpha" || got.ConsumerKeyID != "12345678" {
		t.Fatalf("consumer audit = %q/%q, want alpha/12345678", got.ConsumerName, got.ConsumerKeyID)
	}
	if got.Kind != llmtypes.KindForbidden || got.StatusCode != http.StatusForbidden {
		t.Fatalf("Kind/StatusCode = %q/%d, want forbidden/403", got.Kind, got.StatusCode)
	}
	if callRec.len() != 0 {
		t.Fatalf("call records = %d, want 0 for allowlist rejection", callRec.len())
	}
}

func TestAdoptError_NonProviderError_Falls500Unknown(t *testing.T) {
	rec := &telemetry.AuditEvent{}
	adoptError(rec, io.ErrUnexpectedEOF)
	if rec.Kind != llmtypes.KindUnknown {
		t.Errorf("Kind = %q, want unknown", rec.Kind)
	}
	if rec.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want 500", rec.StatusCode)
	}
}

func TestAdoptStreamSummary_FinalizesAttemptAndRecord(t *testing.T) {
	started := time.Unix(1700000000, 0)
	now := started.Add(250 * time.Millisecond)
	call := &telemetry.CallEvent{
		EventCommon: telemetry.EventCommon{StatusCode: http.StatusOK},
		Attempts: []llmtypes.Attempt{
			{Vendor: "v", Model: "m", StartedAt: started},
		},
	}
	sum := &llmtypes.Summary{
		Usage:      &llmtypes.Usage{PromptTokens: 5, CompletionTokens: 7, TotalTokens: 12},
		VendorCost: `"0.001"`,
	}

	telemetry.AdoptStreamSummary(call, sum, now)

	if call.Usage == nil || call.Usage.TotalTokens != 12 {
		t.Errorf("call.Usage = %+v, want total=12", call.Usage)
	}
	if call.VendorCost != `"0.001"` {
		t.Errorf("call.VendorCost = %q, want \"0.001\"", call.VendorCost)
	}
	last := call.Attempts[0]
	if last.DurationMS != 250 {
		t.Errorf("last.DurationMS = %d, want 250", last.DurationMS)
	}
	if last.StatusCode != http.StatusOK {
		t.Errorf("last.StatusCode = %d, want 200 propagated", last.StatusCode)
	}
	if last.Usage == nil || last.Usage.TotalTokens != 12 {
		t.Errorf("last.Usage = %+v, want total=12 propagated", last.Usage)
	}
	if last.VendorCost != `"0.001"` {
		t.Errorf("last.VendorCost = %q, want \"0.001\" propagated", last.VendorCost)
	}
}

func TestAdoptStreamSummary_PropagatesRecvErrorKindToAttempt(t *testing.T) {
	// Recv loop set rec.Kind; the deferred summary sync must mirror
	// it onto the in-flight Attempt so audit logs stay symmetric with the
	// non-stream path.
	started := time.Unix(1700000000, 0)
	now := started.Add(100 * time.Millisecond)
	call := &telemetry.CallEvent{
		EventCommon: telemetry.EventCommon{Kind: llmtypes.KindUpstream},
		Attempts: []llmtypes.Attempt{
			{Vendor: "v", Model: "m", StartedAt: started},
		},
	}

	telemetry.AdoptStreamSummary(call, nil, now)

	if call.Attempts[0].Kind != llmtypes.KindUpstream {
		t.Errorf("attempt ErrorKind = %q, want upstream", call.Attempts[0].Kind)
	}
	if call.Attempts[0].DurationMS != 100 {
		t.Errorf("DurationMS = %d, want 100", call.Attempts[0].DurationMS)
	}
}

// fakeService implements ChatService for handler tests. buildResult /
// buildStreamResult let each test case shape the RouteResult —
// including pre-populated Attempts — so we exercise the audit-copy
// path without spinning up a real Service.
type fakeService struct {
	vendor            string
	buildResult       func(req *llmtypes.Request) *llmrouter.RouteResult
	buildStreamResult func(req *llmtypes.Request) (*llmrouter.RouteResult, error)
}

func (f *fakeService) Complete(_ context.Context, req *llmtypes.Request) (*llmrouter.RouteResult, error) {
	return f.buildResult(req), nil
}

func (f *fakeService) CompleteStream(_ context.Context, req *llmtypes.Request) (*llmrouter.RouteResult, error) {
	if f.buildStreamResult != nil {
		return f.buildStreamResult(req)
	}
	return &llmrouter.RouteResult{}, &llmtypes.Error{
		Kind:    llmtypes.KindUpstream,
		Message: "stream not implemented in this fake",
	}
}

func newTestHandler(
	service ChatService,
	auditSink telemetry.EventSink,
	callSink telemetry.EventSink,
	cfg HandlerConfig,
) *Handler {
	return NewHandler(
		service,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		telemetry.NewFanoutSink(nil, auditSink, callSink),
		cfg,
	)
}

func okFakeService() *fakeService {
	return &fakeService{
		vendor: "opencode",
		buildResult: func(req *llmtypes.Request) *llmrouter.RouteResult {
			return &llmrouter.RouteResult{
				Response: &llmtypes.Response{
					Model:   req.Model,
					Choices: []llmtypes.Choice{{Index: 0, Message: llmtypes.Message{Role: "assistant", Content: "ok"}}},
				},
				Vendor:    "opencode",
				ModelUsed: req.Model,
				Attempts: []llmtypes.Attempt{
					{Vendor: "opencode", Model: req.Model, StatusCode: http.StatusOK, StartedAt: time.Now()},
				},
			}
		},
	}
}

type panicEventSink struct{}

func (panicEventSink) Emit(context.Context, telemetry.Event) { panic("telemetry sink failed") }
func (panicEventSink) Close() error                          { return nil }

type panicLifecycleObserver struct{}

func (panicLifecycleObserver) RequestStarted(context.Context) { panic("request started failed") }
func (panicLifecycleObserver) RequestFinished(context.Context) {
	panic("request finished failed")
}
func (panicLifecycleObserver) StreamStarted(context.Context, telemetry.EventCommon) {
	panic("stream started failed")
}
func (panicLifecycleObserver) StreamFinished(context.Context, *telemetry.AuditEvent, *telemetry.CallEvent) {
	panic("stream finished failed")
}

type captureAuditSink struct {
	mu      sync.Mutex
	records []*telemetry.AuditEvent
}

func newCaptureAuditSink() (*captureAuditSink, telemetry.EventSink) {
	c := &captureAuditSink{}
	return c, c
}

func (c *captureAuditSink) captureAudit(_ context.Context, r *telemetry.AuditEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r)
}

func (c *captureAuditSink) Close() error { return nil }

func (c *captureAuditSink) Emit(ctx context.Context, event telemetry.Event) {
	rec, ok := event.(*telemetry.AuditEvent)
	if !ok {
		return
	}
	c.captureAudit(ctx, rec)
}

func (c *captureAuditSink) last(t *testing.T) *telemetry.AuditEvent {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.records) == 0 {
		t.Fatalf("no records captured")
	}
	return c.records[len(c.records)-1]
}

type captureCallSink struct {
	mu    sync.Mutex
	calls []*telemetry.CallEvent
}

func newCaptureCallSink() (*captureCallSink, telemetry.EventSink) {
	c := &captureCallSink{}
	return c, c
}

func (c *captureCallSink) captureCall(_ context.Context, r *telemetry.CallEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, r)
}

func (c *captureCallSink) Close() error { return nil }

func (c *captureCallSink) Emit(ctx context.Context, event telemetry.Event) {
	rec, ok := event.(*telemetry.CallEvent)
	if !ok {
		return
	}
	c.captureCall(ctx, rec)
}

func (c *captureCallSink) last(t *testing.T) *telemetry.CallEvent {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.calls) == 0 {
		t.Fatalf("no call records captured")
	}
	return c.calls[len(c.calls)-1]
}

func (c *captureCallSink) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func newLogContractSink() (*bytes.Buffer, *bytes.Buffer, telemetry.EventSink) {
	auditBuf := &bytes.Buffer{}
	callBuf := &bytes.Buffer{}
	auditLog := slog.New(slog.NewJSONHandler(auditBuf, nil)).With(slog.String("log", "audit"))
	callLog := slog.New(slog.NewJSONHandler(callBuf, nil)).With(slog.String("log", "call"))
	return auditBuf, callBuf, telemetry.NewSlogSink(auditLog, callLog)
}

func decodeSingleLogLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 || lines[0] == "" {
		t.Fatalf("log lines = %d, want 1; logs=%q", len(lines), buf.String())
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &out); err != nil {
		t.Fatalf("decode log line: %v; line=%s", err, lines[0])
	}
	return out
}

func requestWithTelemetryContext(req *http.Request, requestID string, consumer *ConsumerInfo) *http.Request {
	ctx := context.WithValue(req.Context(), requestIDCtxKey{}, requestID)
	ctx = context.WithValue(ctx, consumerCtxKey{}, consumer)
	return req.WithContext(ctx)
}

func assertSuccessAuditLog(t *testing.T, got map[string]any, requestID, operation string) {
	t.Helper()
	wantLogField(t, got, "log", "audit")
	wantLogField(t, got, "event_type", "audit")
	wantLogField(t, got, "request_id", requestID)
	wantLogField(t, got, "operation", operation)
	wantLogNumber(t, got, "status", http.StatusOK)
	wantLogField(t, got, "consumer_name", "example")
	wantLogField(t, got, "consumer_key_id", "467d813a")
	wantLogField(t, got, "auth_result", "success")
	wantLogField(t, got, "policy_result", "allowed")
	wantLogField(t, got, "resource_type", "llm_model")
	wantLogField(t, got, "resource_id", "deepseek-v4-flash")
}

func assertSuccessCallLog(t *testing.T, got map[string]any, requestID, operation string) {
	t.Helper()
	wantLogField(t, got, "log", "call")
	wantLogField(t, got, "event_type", "call")
	wantLogField(t, got, "request_id", requestID)
	wantLogField(t, got, "operation", operation)
	wantLogNumber(t, got, "status", http.StatusOK)
	wantLogField(t, got, "consumer_name", "example")
	wantLogField(t, got, "consumer_key_id", "467d813a")
	wantLogField(t, got, "model_requested", "deepseek-v4-flash")
	wantLogField(t, got, "vendor", "opencode")
	wantLogField(t, got, "final_attempt_vendor", "opencode")
	wantLogField(t, got, "final_attempt_model", "deepseek-v4-flash")
	wantLogNumber(t, got, "attempts_count", 1)
}

func wantLogField(t *testing.T, got map[string]any, key string, want string) {
	t.Helper()
	if got[key] != want {
		t.Fatalf("%s = %v, want %q; log=%+v", key, got[key], want, got)
	}
}

func wantLogNumber(t *testing.T, got map[string]any, key string, want int) {
	t.Helper()
	val, ok := got[key].(float64)
	if !ok {
		t.Fatalf("%s = %T(%v), want number %d; log=%+v", key, got[key], got[key], want, got)
	}
	if int(val) != want {
		t.Fatalf("%s = %v, want %d; log=%+v", key, val, want, got)
	}
}

func assertLogDoesNotContainSensitiveMaterial(t *testing.T, bufs ...*bytes.Buffer) {
	t.Helper()
	var joined strings.Builder
	for _, buf := range bufs {
		joined.WriteString(buf.String())
	}
	logged := joined.String()
	for _, forbidden := range []string{
		"Authorization",
		"Bearer ",
		"example-key-001",
		"Reply with exactly OK.",
		"say ok",
	} {
		if strings.Contains(logged, forbidden) {
			t.Fatalf("log leaked %q: %s", forbidden, logged)
		}
	}
}

type captureLifecycle struct {
	requestStarted  int
	requestFinished int
	streamStarted   int
	streamFinished  int
	streamCommon    telemetry.EventCommon
	streamAudit     *telemetry.AuditEvent
	streamCall      *telemetry.CallEvent
}

func (c *captureLifecycle) RequestStarted(context.Context) {
	c.requestStarted++
}

func (c *captureLifecycle) RequestFinished(context.Context) {
	c.requestFinished++
}

func (c *captureLifecycle) StreamStarted(_ context.Context, common telemetry.EventCommon) {
	c.streamStarted++
	c.streamCommon = common
}

func (c *captureLifecycle) StreamFinished(_ context.Context, audit *telemetry.AuditEvent, call *telemetry.CallEvent) {
	c.streamFinished++
	c.streamAudit = audit
	c.streamCall = call
}
