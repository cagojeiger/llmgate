package server

import (
	"net/http"
	"testing"
	"time"

	"llmgate/internal/llmrouter"
	"llmgate/internal/llmtypes"
	"llmgate/internal/providers/fake"
)

func TestHandler_LLMResult_NonStreamFinalized(t *testing.T) {
	results, resultSink := newCaptureResultSink()
	r := okFakeService()
	h := newHandlerHarness(r, HandlerConfig{ResultSink: resultSink})

	w := h.serve(chatBody)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	got := results.last(t)
	if got.EventType != "llm.result.finalized" {
		t.Fatalf("EventType = %q, want llm.result.finalized", got.EventType)
	}
	if got.Operation != "chat.completions" {
		t.Errorf("Operation = %q, want chat.completions", got.Operation)
	}
	if got.Request == nil || got.Request.Model != "deepseek-v4-flash" {
		t.Fatalf("Request = %+v, want captured model", got.Request)
	}
	if got.Response == nil || len(got.Response.Choices) != 1 {
		t.Fatalf("Response = %+v, want one choice", got.Response)
	}
	if got.Response.Choices[0].Message.Content != "ok" {
		t.Errorf("response content = %q, want ok", got.Response.Choices[0].Message.Content)
	}
	if got.StatusCode != http.StatusOK || got.ModelUsed != "deepseek-v4-flash" || got.Vendor != "opencode" {
		t.Errorf("result routing fields = status:%d vendor:%q model:%q", got.StatusCode, got.Vendor, got.ModelUsed)
	}
	if got.ResponseBytes <= 0 || got.DurationMS < 0 {
		t.Errorf("result accounting = bytes:%d duration:%d", got.ResponseBytes, got.DurationMS)
	}
}

func TestHandler_LLMResult_StreamAssemblesFinalResponse(t *testing.T) {
	results, resultSink := newCaptureResultSink()
	streamObj := fake.NewStream(
		fake.WithEvents([]*llmtypes.Event{
			{
				ID:      "chatcmpl-1",
				Created: 1700000000,
				Model:   "deepseek-v4-flash",
				Choices: []llmtypes.ChoiceDelta{{
					Index: 0,
					Delta: llmtypes.Delta{Role: "assistant", Content: "hello"},
				}},
			},
			{
				ID:    "chatcmpl-1",
				Model: "deepseek-v4-flash",
				Choices: []llmtypes.ChoiceDelta{{
					Index:        0,
					Delta:        llmtypes.Delta{Content: " world"},
					FinishReason: "stop",
				}},
				Usage: &llmtypes.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5},
			},
		}),
		fake.WithSummary(&llmtypes.Summary{
			Usage:       &llmtypes.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5},
			ChunkCount:  2,
			FirstByteAt: time.Now(),
		}),
	)
	r := &fakeService{
		buildStreamResult: func(req *llmtypes.Request) (*llmrouter.RouteResult, error) {
			return streamRouteResult(req, streamObj), nil
		},
	}
	h := newHandlerHarness(r, HandlerConfig{ResultSink: resultSink})

	w := h.serve(streamChatBody)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	got := results.last(t)
	if got.Operation != "chat.completions.stream" {
		t.Errorf("Operation = %q, want chat.completions.stream", got.Operation)
	}
	if got.Response == nil {
		t.Fatal("Response = nil, want assembled stream response")
	}
	if got.Response.ID != "chatcmpl-1" || got.Response.Model != "deepseek-v4-flash" {
		t.Errorf("response identity = %q/%q", got.Response.ID, got.Response.Model)
	}
	if len(got.Response.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(got.Response.Choices))
	}
	choice := got.Response.Choices[0]
	if choice.Message.Role != "assistant" || choice.Message.Content != "hello world" || choice.FinishReason != "stop" {
		t.Errorf("assembled choice = %+v", choice)
	}
	if got.Usage == nil || got.Usage.TotalTokens != 5 {
		t.Errorf("event usage = %+v, want total=5", got.Usage)
	}
	if got.Response.Usage == nil || got.Response.Usage.TotalTokens != 5 {
		t.Errorf("response usage = %+v, want total=5", got.Response.Usage)
	}
	if got.StreamChunks != 2 {
		t.Errorf("StreamChunks = %d, want 2", got.StreamChunks)
	}
}

func TestHandler_LLMResult_StreamErrorOmitsPartialResponse(t *testing.T) {
	results, resultSink := newCaptureResultSink()
	streamObj := fake.NewStream(
		fake.WithEvents([]*llmtypes.Event{
			{Choices: []llmtypes.ChoiceDelta{{Delta: llmtypes.Delta{Content: "partial"}}}},
		}),
		fake.WithRecvErr(&llmtypes.Error{Kind: llmtypes.KindUpstream, Message: "boom mid-stream"}),
		fake.WithSummary(&llmtypes.Summary{ChunkCount: 1}),
	)
	r := &fakeService{
		buildStreamResult: func(req *llmtypes.Request) (*llmrouter.RouteResult, error) {
			return streamRouteResult(req, streamObj), nil
		},
	}
	h := newHandlerHarness(r, HandlerConfig{ResultSink: resultSink})

	w := h.serve(streamChatBody)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 because SSE already started", w.Code)
	}
	got := results.last(t)
	if got.ErrorKind != llmtypes.KindUpstream {
		t.Fatalf("ErrorKind = %q, want upstream", got.ErrorKind)
	}
	if got.Response != nil {
		t.Fatalf("Response = %+v, want nil for incomplete stream", got.Response)
	}
}
