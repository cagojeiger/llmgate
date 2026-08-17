package routing

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"llmgate/internal/domain/llmtypes"
)

// fakeTranscriber is a minimal llmtypes.TranscriptionProvider for routing
// tests: it records the model it was asked to serve and returns a canned
// response or a preset error.
type fakeTranscriber struct {
	name     string
	calls    int
	lastReq  *llmtypes.TranscriptionRequest
	response *llmtypes.TranscriptionResponse
	stream   llmtypes.TranscriptionStream
	err      error
}

func (f *fakeTranscriber) Name() string { return f.name }

func (f *fakeTranscriber) Transcribe(_ context.Context, req *llmtypes.TranscriptionRequest) (*llmtypes.TranscriptionResponse, error) {
	f.calls++
	f.lastReq = req
	if f.err != nil {
		return nil, f.err
	}
	return f.response, nil
}

func (f *fakeTranscriber) TranscribeStream(_ context.Context, req *llmtypes.TranscriptionRequest) (llmtypes.TranscriptionStream, error) {
	f.calls++
	f.lastReq = req
	if f.err != nil {
		return nil, f.err
	}
	return f.stream, nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTranscribeService(t *testing.T, transcribers TranscriptionModels) *Service {
	t.Helper()
	// Transcription-only deployments still need a non-empty Models map because
	// NewService fails fast on zero chat models; a harmless stub satisfies that
	// without affecting the transcription path under test.
	svc, err := NewService(
		Models{"chat-stub": stubChatProvider{}},
		Aliases{"stt": {"qwen-asr"}},
		testPolicy,
		discardLogger(),
		WithTranscription(transcribers),
	)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return svc
}

type stubChatProvider struct{}

func (stubChatProvider) Name() string { return "stub" }
func (stubChatProvider) Complete(context.Context, *llmtypes.Request) (*llmtypes.Response, error) {
	return &llmtypes.Response{}, nil
}
func (stubChatProvider) CompleteStream(context.Context, *llmtypes.Request) (llmtypes.Stream, error) {
	return nil, errors.New("not implemented")
}

func TestService_Transcribe_ResolvesAliasAndRecordsAttempt(t *testing.T) {
	tr := &fakeTranscriber{name: "qwen", response: &llmtypes.TranscriptionResponse{Text: "hello"}}
	svc := newTranscribeService(t, TranscriptionModels{"qwen-asr": tr})

	result, err := svc.Transcribe(context.Background(), &llmtypes.TranscriptionRequest{
		Model: "stt", // alias -> qwen-asr
		Audio: []byte("data"),
	})
	if err != nil {
		t.Fatalf("Transcribe() error = %v", err)
	}
	if result.Response == nil || result.Response.Text != "hello" {
		t.Fatalf("response = %+v, want text hello", result.Response)
	}
	if result.ModelUsed != "qwen-asr" || result.Vendor != "qwen" {
		t.Errorf("routing fields = model:%q vendor:%q, want qwen-asr/qwen", result.ModelUsed, result.Vendor)
	}
	if len(result.Attempts) != 1 || result.Attempts[0].StatusCode != 200 {
		t.Errorf("attempts = %+v, want one 200 attempt", result.Attempts)
	}
	if tr.calls != 1 || tr.lastReq.Model != "qwen-asr" {
		t.Errorf("provider calls = %d, model = %q, want 1 / qwen-asr", tr.calls, tr.lastReq.Model)
	}
}

func TestService_Transcribe_RawModelID(t *testing.T) {
	tr := &fakeTranscriber{name: "qwen", response: &llmtypes.TranscriptionResponse{Text: "ok"}}
	svc := newTranscribeService(t, TranscriptionModels{"qwen-asr": tr})

	result, err := svc.Transcribe(context.Background(), &llmtypes.TranscriptionRequest{Model: "qwen-asr", Audio: []byte("d")})
	if err != nil {
		t.Fatalf("Transcribe() error = %v", err)
	}
	if result.ModelUsed != "qwen-asr" {
		t.Errorf("ModelUsed = %q, want qwen-asr", result.ModelUsed)
	}
}

func TestService_Transcribe_UnknownModel(t *testing.T) {
	tr := &fakeTranscriber{name: "qwen", response: &llmtypes.TranscriptionResponse{Text: "ok"}}
	svc := newTranscribeService(t, TranscriptionModels{"qwen-asr": tr})

	_, err := svc.Transcribe(context.Background(), &llmtypes.TranscriptionRequest{Model: "does-not-exist", Audio: []byte("d")})
	var perr *llmtypes.Error
	if !errors.As(err, &perr) || perr.Kind != llmtypes.KindBadRequest {
		t.Fatalf("error = %v, want bad_request for unknown model", err)
	}
}

func TestService_Transcribe_ChatModelNotReachable(t *testing.T) {
	// A chat-only model id must not resolve on the transcription surface.
	tr := &fakeTranscriber{name: "qwen", response: &llmtypes.TranscriptionResponse{Text: "ok"}}
	svc := newTranscribeService(t, TranscriptionModels{"qwen-asr": tr})

	_, err := svc.Transcribe(context.Background(), &llmtypes.TranscriptionRequest{Model: "chat-stub", Audio: []byte("d")})
	var perr *llmtypes.Error
	if !errors.As(err, &perr) || perr.Kind != llmtypes.KindBadRequest {
		t.Fatalf("error = %v, want bad_request for chat model on transcription surface", err)
	}
}

// stubTranscriptionStream is a no-op llmtypes.TranscriptionStream for routing
// tests: TranscribeStream only needs a non-nil stream to record a success.
type stubTranscriptionStream struct{}

func (stubTranscriptionStream) Recv() (*llmtypes.TranscriptionEvent, error) { return nil, io.EOF }
func (stubTranscriptionStream) Close() error                               { return nil }
func (stubTranscriptionStream) Summary() *llmtypes.Summary                 { return &llmtypes.Summary{} }

func TestService_TranscribeStream_ResolvesAliasAndReturnsStream(t *testing.T) {
	tr := &fakeTranscriber{name: "qwen", stream: stubTranscriptionStream{}}
	svc := newTranscribeService(t, TranscriptionModels{"qwen-asr": tr})

	result, err := svc.TranscribeStream(context.Background(), &llmtypes.TranscriptionRequest{
		Model: "stt", // alias -> qwen-asr
		Audio: []byte("data"),
	})
	if err != nil {
		t.Fatalf("TranscribeStream() error = %v", err)
	}
	if result.Stream == nil {
		t.Fatal("result.Stream = nil, want open stream")
	}
	if result.ModelUsed != "qwen-asr" || result.Vendor != "qwen" {
		t.Errorf("routing fields = model:%q vendor:%q, want qwen-asr/qwen", result.ModelUsed, result.Vendor)
	}
	if len(result.Attempts) != 1 || tr.lastReq.Model != "qwen-asr" {
		t.Errorf("attempts = %+v, provider model = %q, want 1 / qwen-asr", result.Attempts, tr.lastReq.Model)
	}
}

func TestService_TranscribeStream_PropagatesPreStreamError(t *testing.T) {
	tr := &fakeTranscriber{name: "qwen", err: &llmtypes.Error{Kind: llmtypes.KindUpstream, Message: "stream setup failed"}}
	svc := newTranscribeService(t, TranscriptionModels{"qwen-asr": tr})

	result, err := svc.TranscribeStream(context.Background(), &llmtypes.TranscriptionRequest{Model: "qwen-asr", Audio: []byte("d")})
	if err == nil {
		t.Fatal("TranscribeStream() error = nil, want pre-stream error")
	}
	if len(result.Attempts) != 1 || result.Attempts[0].Kind != llmtypes.KindUpstream {
		t.Errorf("attempts = %+v, want one upstream attempt", result.Attempts)
	}
}

func TestService_Transcribe_PropagatesProviderError(t *testing.T) {
	tr := &fakeTranscriber{name: "qwen", err: &llmtypes.Error{Kind: llmtypes.KindUpstream, Message: "boom"}}
	svc := newTranscribeService(t, TranscriptionModels{"qwen-asr": tr})

	result, err := svc.Transcribe(context.Background(), &llmtypes.TranscriptionRequest{Model: "qwen-asr", Audio: []byte("d")})
	if err == nil {
		t.Fatal("Transcribe() error = nil, want provider error")
	}
	if len(result.Attempts) != 1 || result.Attempts[0].Kind != llmtypes.KindUpstream {
		t.Errorf("attempts = %+v, want one upstream attempt", result.Attempts)
	}
}
