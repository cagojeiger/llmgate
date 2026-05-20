package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestHandler_FinalizedEvent_NonStreamIncludesOpenAIWirePayload(t *testing.T) {
	finalized, sink := newCaptureFinalizedSink()
	r := &fakeService{
		buildResult: func(req *llmtypes.Request) *llmrouter.RouteResult {
			return &llmrouter.RouteResult{
				Response: &llmtypes.Response{
					ID:     "chatcmpl-test",
					Object: "chat.completion",
					Model:  req.Model,
					Choices: []llmtypes.Choice{{
						Index:        0,
						Message:      llmtypes.Message{Role: "assistant", Content: "pong"},
						FinishReason: "stop",
					}},
					Usage: &llmtypes.Usage{PromptTokens: 3, CompletionTokens: 1, TotalTokens: 4},
					Extra: map[string]json.RawMessage{"cost": json.RawMessage(`"0.0001"`)},
				},
				Vendor:    "opencode",
				ModelUsed: req.Model,
				Attempts:  []llmtypes.Attempt{{Vendor: "opencode", Model: req.Model, StatusCode: http.StatusOK, StartedAt: time.Now()}},
			}
		},
	}
	h := newTestHandlerWithEventSink(r, sink, HandlerConfig{})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(chatBody))
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	got := finalized.last(t)
	if got.EventType != telemetry.EventTypeLLMCallFinalized || got.WireFormat != telemetry.LLMCallFinalizedWireFormat {
		t.Fatalf("event identity = %q/%q", got.EventType, got.WireFormat)
	}
	if got.Status != telemetry.LLMCallStatusSuccess {
		t.Fatalf("status = %q, want success", got.Status)
	}
	if got.CompletedAt == "" || got.DurationMS < 0 {
		t.Fatalf("completion timing = completed_at:%q duration_ms:%d", got.CompletedAt, got.DurationMS)
	}
	if !got.Request.Available || !got.Response.Available {
		t.Fatalf("raw availability request=%v response=%v", got.Request.Available, got.Response.Available)
	}
	var rawReq map[string]any
	if err := json.Unmarshal(got.Request.RawJSON, &rawReq); err != nil {
		t.Fatalf("request raw json: %v", err)
	}
	if rawReq["model"] != "deepseek-v4-flash" {
		t.Fatalf("request model = %v", rawReq["model"])
	}
	var rawResp map[string]any
	if err := json.Unmarshal(got.Response.RawJSON, &rawResp); err != nil {
		t.Fatalf("response raw json: %v", err)
	}
	if rawResp["cost"] != "0.0001" {
		t.Fatalf("response cost extra = %v, want preserved", rawResp["cost"])
	}
	if got.Routing.Vendor != "opencode" || got.Routing.ModelUsed != "deepseek-v4-flash" {
		t.Fatalf("routing = %+v", got.Routing)
	}
	if got.Usage == nil || got.Usage.TotalTokens != 4 {
		t.Fatalf("usage = %+v, want total=4", got.Usage)
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
		Usage:       &llmtypes.Usage{PromptTokens: 5, CompletionTokens: 7, TotalTokens: 12},
		VendorCost:  `"0.001"`,
		ChunkCount:  3,
		FirstByteAt: started.Add(75 * time.Millisecond),
	}

	telemetry.AdoptStreamSummary(call, sum, now)

	if call.Usage == nil || call.Usage.TotalTokens != 12 {
		t.Errorf("call.Usage = %+v, want total=12", call.Usage)
	}
	if call.VendorCost != `"0.001"` {
		t.Errorf("call.VendorCost = %q, want \"0.001\"", call.VendorCost)
	}
	if call.StreamChunks != 3 {
		t.Errorf("call.StreamChunks = %d, want 3", call.StreamChunks)
	}
	if call.FirstByteMS != 75 {
		t.Errorf("call.FirstByteMS = %d, want 75", call.FirstByteMS)
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
