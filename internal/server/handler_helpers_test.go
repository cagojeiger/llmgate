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

	"llmgate/internal/domain/llmresult"
	llmresultsink "llmgate/internal/domain/llmresult/sink"
	"llmgate/internal/domain/routing"
	"llmgate/internal/llmtypes"
	"llmgate/internal/telemetry"
)

const (
	chatCompletionsPath = "/v1/chat/completions"
	chatBody            = `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`
	streamChatBody      = `{"model":"deepseek-v4-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`
)

type handlerHarness struct {
	handler *Handler
	audit   *captureAuditSink
	calls   *captureCallSink
}

func newHandlerHarness(service ChatService, cfg HandlerConfig) *handlerHarness {
	audit, auditSink := newCaptureAuditSink()
	calls, callSink := newCaptureCallSink()
	return &handlerHarness{
		handler: newTestHandler(service, auditSink, callSink, cfg),
		audit:   audit,
		calls:   calls,
	}
}

func (h *handlerHarness) serve(body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(body))
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, req)
	return w
}

func (h *handlerHarness) serveRequest(req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, req)
	return w
}

func streamRouteResult(req *llmtypes.Request, stream llmtypes.Stream, attempts ...llmtypes.Attempt) *routing.RouteResult {
	if len(attempts) == 0 {
		attempts = []llmtypes.Attempt{{Vendor: "opencode", Model: req.Model, StartedAt: time.Now()}}
	}
	return &routing.RouteResult{
		Stream:    stream,
		Vendor:    "opencode",
		ModelUsed: req.Model,
		Attempts:  attempts,
	}
}

// fakeService implements ChatService for handler tests. buildResult /
// buildStreamResult let each test case shape the RouteResult —
// including pre-populated Attempts — so we exercise the audit-copy
// path without spinning up a real Service.
type fakeService struct {
	vendor            string
	buildResult       func(req *llmtypes.Request) *routing.RouteResult
	buildStreamResult func(req *llmtypes.Request) (*routing.RouteResult, error)
}

func (f *fakeService) Complete(_ context.Context, req *llmtypes.Request) (*routing.RouteResult, error) {
	return f.buildResult(req), nil
}

func (f *fakeService) CompleteStream(_ context.Context, req *llmtypes.Request) (*routing.RouteResult, error) {
	if f.buildStreamResult != nil {
		return f.buildStreamResult(req)
	}
	return &routing.RouteResult{}, &llmtypes.Error{
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
		buildResult: func(req *llmtypes.Request) *routing.RouteResult {
			return &routing.RouteResult{
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

type captureResultSink struct {
	mu      sync.Mutex
	records []*llmresult.Event
}

func newCaptureResultSink() (*captureResultSink, llmresultsink.Sink) {
	c := &captureResultSink{}
	return c, c
}

func (c *captureResultSink) Emit(_ context.Context, event *llmresult.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, event)
}

func (c *captureResultSink) Close() error { return nil }

func (c *captureResultSink) last(t *testing.T) *llmresult.Event {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.records) == 0 {
		t.Fatalf("no llm result records captured")
	}
	return c.records[len(c.records)-1]
}

func (c *captureResultSink) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.records)
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
