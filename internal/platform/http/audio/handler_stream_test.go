package audio

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/routing"
)

// fakeTranscriptionStream is a deterministic llmtypes.TranscriptionStream for
// handler tests: it replays canned events, then returns recvErr (or io.EOF),
// and records Close calls.
type fakeTranscriptionStream struct {
	events  []*llmtypes.TranscriptionEvent
	idx     int
	closed  int
	recvErr error
	summary *llmtypes.Summary
}

func (s *fakeTranscriptionStream) Recv() (*llmtypes.TranscriptionEvent, error) {
	if s.idx < len(s.events) {
		e := s.events[s.idx]
		s.idx++
		return e, nil
	}
	if s.recvErr != nil {
		return nil, s.recvErr
	}
	return nil, io.EOF
}

func (s *fakeTranscriptionStream) Close() error { s.closed++; return nil }

func (s *fakeTranscriptionStream) Summary() *llmtypes.Summary {
	if s.summary != nil {
		return s.summary
	}
	return &llmtypes.Summary{}
}

func streamService(stream llmtypes.TranscriptionStream) *fakeService {
	return &fakeService{
		buildStream: func(req *llmtypes.TranscriptionRequest) (*routing.TranscribeResult, error) {
			return &routing.TranscribeResult{
				Stream:    stream,
				Vendor:    "qwen",
				ModelUsed: "qwen-asr",
				Attempts: []llmtypes.Attempt{
					{Vendor: "qwen", Model: "qwen-asr", StartedAt: time.Now()},
				},
			}, nil
		},
	}
}

func TestHandler_Stream_RelaysEventsAndFullAudit(t *testing.T) {
	stream := &fakeTranscriptionStream{
		events: []*llmtypes.TranscriptionEvent{
			{Type: "transcript.text.delta", Delta: "he", Raw: []byte(`{"type":"transcript.text.delta","delta":"he"}`)},
			{Type: "transcript.text.delta", Delta: "llo", Raw: []byte(`{"type":"transcript.text.delta","delta":"llo"}`)},
			{Type: "transcript.text.done", Text: "hello", Raw: []byte(`{"type":"transcript.text.done","text":"hello"}`)},
		},
	}
	h := newHarness(t, streamService(stream))
	audio := []byte("RIFFxxxxWAVE")
	w := h.serve(t, map[string]string{"model": "stt", "stream": "true"}, "clip.wav", audio)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}

	body := w.Body.String()
	if !strings.Contains(body, `data: {"type":"transcript.text.delta","delta":"he"}`) {
		t.Errorf("body missing first delta frame: %q", body)
	}
	if !strings.Contains(body, `data: {"type":"transcript.text.done","text":"hello"}`) {
		t.Errorf("body missing done frame: %q", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("body must end with [DONE] frame: %q", body)
	}
	if stream.closed != 1 {
		t.Errorf("Stream.Close() calls = %d, want 1", stream.closed)
	}

	// Audit records the streaming operation and a 200.
	audit := h.events.lastAudit(t)
	if audit.Operation != "audio.transcriptions.stream" || audit.StatusCode != http.StatusOK {
		t.Errorf("audit = op:%q status:%d, want audio.transcriptions.stream/200", audit.Operation, audit.StatusCode)
	}

	// The result record still carries the FULL audio + assembled transcript,
	// exactly like the non-stream path.
	rec := h.results.last(t)
	if rec.Operation != "audio.transcriptions.stream" || rec.ModelUsed != "qwen-asr" || rec.Vendor != "qwen" {
		t.Errorf("result routing = op:%q model:%q vendor:%q", rec.Operation, rec.ModelUsed, rec.Vendor)
	}
	if rec.TranscriptionResponse == nil || rec.TranscriptionResponse.Text != "hello" {
		t.Errorf("assembled response = %+v, want full text hello", rec.TranscriptionResponse)
	}
	if rec.TranscriptionRequest == nil || string(rec.TranscriptionRequest.Audio) != string(audio) {
		t.Errorf("captured audio = %+v, want %q", rec.TranscriptionRequest, audio)
	}
	if rec.RequestBytes != int64(len(audio)) {
		t.Errorf("RequestBytes = %d, want %d (audio size)", rec.RequestBytes, len(audio))
	}
}

// When no terminal done event arrives, the assembled transcript falls back to
// the concatenated deltas.
func TestHandler_Stream_AssemblesFromDeltasWhenNoDone(t *testing.T) {
	stream := &fakeTranscriptionStream{
		events: []*llmtypes.TranscriptionEvent{
			{Type: "transcript.text.delta", Delta: "foo ", Raw: []byte(`{"type":"transcript.text.delta","delta":"foo "}`)},
			{Type: "transcript.text.delta", Delta: "bar", Raw: []byte(`{"type":"transcript.text.delta","delta":"bar"}`)},
		},
	}
	h := newHarness(t, streamService(stream))
	w := h.serve(t, map[string]string{"model": "stt", "stream": "true"}, "clip.wav", []byte("d"))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	rec := h.results.last(t)
	if rec.TranscriptionResponse == nil || rec.TranscriptionResponse.Text != "foo bar" {
		t.Errorf("assembled response = %+v, want concatenated deltas 'foo bar'", rec.TranscriptionResponse)
	}
}

// A mid-stream error rides as an SSE frame after the headers already flushed,
// then [DONE] terminates; the audit kind is propagated.
func TestHandler_Stream_MidStreamErrorPropagatesKind(t *testing.T) {
	stream := &fakeTranscriptionStream{
		events: []*llmtypes.TranscriptionEvent{
			{Type: "transcript.text.delta", Delta: "partial", Raw: []byte(`{"type":"transcript.text.delta","delta":"partial"}`)},
		},
		recvErr: &llmtypes.Error{Kind: llmtypes.KindUpstream, Message: "boom mid-stream"},
	}
	h := newHarness(t, streamService(stream))
	w := h.serve(t, map[string]string{"model": "stt", "stream": "true"}, "clip.wav", []byte("d"))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (headers already flushed)", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"partial"`) {
		t.Errorf("body missing pre-error frame: %q", body)
	}
	if !strings.Contains(body, `"type":"upstream"`) {
		t.Errorf("body missing upstream error envelope: %q", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("body must end with [DONE]: %q", body)
	}
	if audit := h.events.lastAudit(t); audit.Kind != llmtypes.KindUpstream {
		t.Errorf("audit.Kind = %q, want upstream", audit.Kind)
	}
}
