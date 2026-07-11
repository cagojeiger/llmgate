package chat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/routing"
	"llmgate/internal/domain/telemetry"
)

// trackingService records whether Complete/CompleteStream ran so the test
// can assert handler-side validation short-circuits before the service is
// reached.
type trackingService struct {
	completeCalls int
	streamCalls   int
}

func (s *trackingService) Complete(context.Context, *llmtypes.Request) (*routing.RouteResult, error) {
	s.completeCalls++
	return &routing.RouteResult{}, nil
}

func (s *trackingService) CompleteStream(context.Context, *llmtypes.Request) (*routing.RouteResult, error) {
	s.streamCalls++
	return &routing.RouteResult{}, nil
}

func TestServeHTTP_EmptyMessages_RejectedBeforeService(t *testing.T) {
	svc := &trackingService{}
	_, audit := newCaptureAuditSink()
	_, call := newCaptureCallSink()
	h := newTestHandler(svc, audit, call, HandlerConfig{})

	body := `{"model":"deepseek-v4-flash","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if svc.completeCalls != 0 || svc.streamCalls != 0 {
		t.Errorf("service reached on empty-messages: complete=%d stream=%d", svc.completeCalls, svc.streamCalls)
	}
}

func TestServeHTTP_MissingModel_RejectedBeforeService(t *testing.T) {
	svc := &trackingService{}
	_, audit := newCaptureAuditSink()
	_, call := newCaptureCallSink()
	h := newTestHandler(svc, audit, call, HandlerConfig{})

	body := `{"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if svc.completeCalls != 0 || svc.streamCalls != 0 {
		t.Errorf("service reached on missing-model: complete=%d stream=%d", svc.completeCalls, svc.streamCalls)
	}
}

func TestServeHTTP_EmptyMessages_AuditStampedBadRequest(t *testing.T) {
	svc := &trackingService{}
	auditRec, audit := newCaptureAuditSink()
	_, call := newCaptureCallSink()
	h := newTestHandler(svc, audit, call, HandlerConfig{})

	body := `{"model":"deepseek-v4-flash","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	got := auditRec.last(t)
	if got.StatusCode != http.StatusBadRequest {
		t.Errorf("audit StatusCode = %d, want 400", got.StatusCode)
	}
	if got.Kind != llmtypes.KindBadRequest {
		t.Errorf("audit Kind = %q, want %q", got.Kind, llmtypes.KindBadRequest)
	}
	// Policy stamp should still reflect that the request never reached
	// the model-allow check, because validation rejected it first.
	if got.PolicyResult == telemetry.PolicyResultAllowed {
		t.Errorf("PolicyResult = %q, want anything but allowed", got.PolicyResult)
	}
}

func TestHandler_RejectsBodyOverConfiguredCap(t *testing.T) {
	svc := &trackingService{}
	h := newHandlerHarness(svc, HandlerConfig{MaxRequestBytes: 16})

	w := h.serve(chatBody) // chatBody is well over 16 bytes

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "LLMGATE_MAX_REQUEST_BYTES") {
		t.Errorf("body = %s, want cap hint", w.Body.String())
	}
	if svc.completeCalls != 0 || svc.streamCalls != 0 {
		t.Errorf("service reached (%d/%d), want short-circuit at decode", svc.completeCalls, svc.streamCalls)
	}
}

func TestHandler_DefaultCapAcceptsMultiMBBody(t *testing.T) {
	// The default cap must admit image-sized bodies (the old 1 MiB cap
	// rejected them): a ~3 MB message must reach the service.
	r := okFakeService()
	h := newHandlerHarness(r, HandlerConfig{})

	big := `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"` +
		strings.Repeat("a", 3<<20) + `"}]}`
	w := h.serve(big)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String()[:min(200, w.Body.Len())])
	}
}
