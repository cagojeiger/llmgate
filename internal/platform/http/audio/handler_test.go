package audio

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	llmresult "llmgate/internal/domain/llmresult/schema"
	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/routing"
	"llmgate/internal/domain/telemetry"
	httpauth "llmgate/internal/platform/http/auth"
)

const transcriptionsPath = "/v1/audio/transcriptions"

// --- fakes ---

type fakeService struct {
	build       func(req *llmtypes.TranscriptionRequest) (*routing.TranscribeResult, error)
	buildStream func(req *llmtypes.TranscriptionRequest) (*routing.TranscribeResult, error)
}

func (f *fakeService) Transcribe(_ context.Context, req *llmtypes.TranscriptionRequest) (*routing.TranscribeResult, error) {
	return f.build(req)
}

func (f *fakeService) TranscribeStream(_ context.Context, req *llmtypes.TranscriptionRequest) (*routing.TranscribeResult, error) {
	return f.buildStream(req)
}

func okService() *fakeService {
	return &fakeService{
		build: func(req *llmtypes.TranscriptionRequest) (*routing.TranscribeResult, error) {
			return &routing.TranscribeResult{
				Response:  &llmtypes.TranscriptionResponse{Text: "transcribed text"},
				Vendor:    "qwen",
				ModelUsed: "qwen-asr",
				Attempts: []llmtypes.Attempt{
					{Vendor: "qwen", Model: "qwen-asr", StatusCode: http.StatusOK},
				},
			}, nil
		},
	}
}

type captureEventSink struct {
	mu     sync.Mutex
	audits []*telemetry.AuditEvent
	calls  []*telemetry.CallEvent
}

func (c *captureEventSink) Emit(_ context.Context, event telemetry.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch ev := event.(type) {
	case *telemetry.AuditEvent:
		c.audits = append(c.audits, ev)
	case *telemetry.CallEvent:
		c.calls = append(c.calls, ev)
	}
}
func (c *captureEventSink) Close() error { return nil }

func (c *captureEventSink) lastAudit(t *testing.T) *telemetry.AuditEvent {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.audits) == 0 {
		t.Fatal("no audit events captured")
	}
	return c.audits[len(c.audits)-1]
}

type captureResultSink struct {
	mu      sync.Mutex
	records []*llmresult.Event
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
		t.Fatal("no result events captured")
	}
	return c.records[len(c.records)-1]
}

// --- harness ---

type harness struct {
	handler *Handler
	events  *captureEventSink
	results *captureResultSink
}

func newHarness(t *testing.T, svc TranscribeService) *harness {
	t.Helper()
	events := &captureEventSink{}
	results := &captureResultSink{}
	h := NewHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil)), events, HandlerConfig{
		ResultSink: results,
	})
	return &harness{handler: h, events: events, results: results}
}

func multipartBody(t *testing.T, fields map[string]string, filename string, audio []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if filename != "" || audio != nil {
		part, err := mw.CreateFormFile("file", filename)
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		_, _ = part.Write(audio)
	}
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatalf("write field: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

func (h *harness) serve(t *testing.T, fields map[string]string, filename string, audio []byte) *httptest.ResponseRecorder {
	t.Helper()
	body, contentType := multipartBody(t, fields, filename, audio)
	req := httptest.NewRequest(http.MethodPost, transcriptionsPath, body)
	req.Header.Set("Content-Type", contentType)
	// Simulate the auth middleware having authenticated a consumer.
	req = req.WithContext(httpauth.WithConsumer(req.Context(), &httpauth.ConsumerInfo{Name: "alice", KeyID: "abcd1234"}))
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, req)
	return w
}

// --- tests ---

func TestHandler_JSONResponseAndFullAudit(t *testing.T) {
	h := newHarness(t, okService())
	audio := []byte("RIFFxxxxWAVE")
	w := h.serve(t, map[string]string{"model": "stt", "language": "en"}, "clip.wav", audio)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if out["text"] != "transcribed text" {
		t.Errorf("body text = %v, want transcribed text", out["text"])
	}

	audit := h.events.lastAudit(t)
	if audit.Operation != "audio.transcriptions" || audit.StatusCode != http.StatusOK {
		t.Errorf("audit = op:%q status:%d", audit.Operation, audit.StatusCode)
	}
	if audit.ResourceType != "llm_model" || audit.ResourceID != "stt" {
		t.Errorf("audit resource = %q/%q, want llm_model/stt", audit.ResourceType, audit.ResourceID)
	}
	if audit.AuthResult != telemetry.AuthResultSuccess || audit.PolicyResult != telemetry.PolicyResultAllowed {
		t.Errorf("audit decisions = %q/%q", audit.AuthResult, audit.PolicyResult)
	}

	// The result event must carry the FULL request (incl. audio bytes) and the
	// transcribed text, mirroring how chat records prompt+response.
	rec := h.results.last(t)
	if rec.Operation != "audio.transcriptions" || rec.ModelUsed != "qwen-asr" || rec.Vendor != "qwen" {
		t.Errorf("result routing = op:%q model:%q vendor:%q", rec.Operation, rec.ModelUsed, rec.Vendor)
	}
	if rec.TranscriptionRequest == nil {
		t.Fatal("result TranscriptionRequest = nil, want captured request")
	}
	if rec.TranscriptionRequest.Model != "stt" || rec.TranscriptionRequest.Language != "en" || rec.TranscriptionRequest.Filename != "clip.wav" {
		t.Errorf("captured request = %+v", rec.TranscriptionRequest)
	}
	if !bytes.Equal(rec.TranscriptionRequest.Audio, audio) {
		t.Errorf("captured audio = %q, want %q", rec.TranscriptionRequest.Audio, audio)
	}
	if rec.TranscriptionResponse == nil || rec.TranscriptionResponse.Text != "transcribed text" {
		t.Errorf("captured response = %+v", rec.TranscriptionResponse)
	}
	if rec.RequestBytes != int64(len(audio)) {
		t.Errorf("RequestBytes = %d, want %d (audio size)", rec.RequestBytes, len(audio))
	}

	// The audit record must serialize the audio as base64 (json tag present).
	line, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal result event: %v", err)
	}
	if !strings.Contains(string(line), "\"audio\":") {
		t.Errorf("serialized event missing base64 audio field: %s", line)
	}
}

func TestHandler_TextResponseFormat(t *testing.T) {
	h := newHarness(t, okService())
	w := h.serve(t, map[string]string{"model": "stt", "response_format": "text"}, "a.wav", []byte("d"))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q, want text/plain", ct)
	}
	if got := w.Body.String(); got != "transcribed text" {
		t.Errorf("body = %q, want plain transcript", got)
	}
}

func TestHandler_MissingFile_BadRequest(t *testing.T) {
	h := newHarness(t, okService())
	w := h.serve(t, map[string]string{"model": "stt"}, "", nil)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "file is required") {
		t.Errorf("body = %s, want file-required", w.Body.String())
	}
}

func TestHandler_MissingModel_BadRequest(t *testing.T) {
	h := newHarness(t, okService())
	w := h.serve(t, map[string]string{}, "a.wav", []byte("d"))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "model is required") {
		t.Errorf("body = %s, want model-required", w.Body.String())
	}
}

func TestHandler_AuthRequired(t *testing.T) {
	events := &captureEventSink{}
	results := &captureResultSink{}
	h := NewHandler(okService(), slog.New(slog.NewTextHandler(io.Discard, nil)), events, HandlerConfig{ResultSink: results})

	body, contentType := multipartBody(t, map[string]string{"model": "stt"}, "a.wav", []byte("d"))
	req := httptest.NewRequest(http.MethodPost, transcriptionsPath, body)
	req.Header.Set("Content-Type", contentType)
	// Simulate the auth middleware having rejected the request.
	req = req.WithContext(httpauth.WithConsumer(req.Context(), &httpauth.ConsumerInfo{AuthError: telemetry.AuthErrorMissing}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	audit := events.lastAudit(t)
	if audit.AuthResult != telemetry.AuthResultFailure || audit.AuthError != telemetry.AuthErrorMissing {
		t.Errorf("audit auth = %q/%q, want failure/missing", audit.AuthResult, audit.AuthError)
	}
	if len(results.records) != 0 {
		t.Errorf("result events = %d, want 0 (no upstream call attempted)", len(results.records))
	}
}

func TestHandler_ModelNotAllowed_Forbidden(t *testing.T) {
	events := &captureEventSink{}
	results := &captureResultSink{}
	h := NewHandler(okService(), slog.New(slog.NewTextHandler(io.Discard, nil)), events, HandlerConfig{ResultSink: results})

	body, contentType := multipartBody(t, map[string]string{"model": "stt"}, "a.wav", []byte("d"))
	req := httptest.NewRequest(http.MethodPost, transcriptionsPath, body)
	req.Header.Set("Content-Type", contentType)
	req = req.WithContext(httpauth.WithConsumer(req.Context(), &httpauth.ConsumerInfo{
		Name:           "alice",
		KeyID:          "abcd1234",
		AllowedAliases: []string{"other"},
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	audit := events.lastAudit(t)
	if audit.DenyReason != telemetry.DenyReasonModelNotAllowed {
		t.Errorf("deny reason = %q, want model_not_allowed", audit.DenyReason)
	}
}
