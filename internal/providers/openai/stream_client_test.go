package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"llmgate/internal/llmtypes"
)

func TestCompleteStream_Success(t *testing.T) {
	server := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream", got)
		}

		body, _ := io.ReadAll(r.Body)
		var raw map[string]any
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}
		if raw["stream"] != true {
			t.Fatalf("stream = %v, want true", raw["stream"])
		}

		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEChunk(t, w, `{"id":"chat-1","choices":[{"index":0,"delta":{"role":"assistant","content":"hel"}}]}`)
		writeSSEChunk(t, w, `{"id":"chat-1","choices":[{"index":0,"delta":{"content":"lo","reasoning_content":"r1"}}]}`)
		writeSSEChunk(t, w, `
		{
			"id": "chat-1",
			"choices": [
				{
					"index": 0,
					"delta": {},
					"finish_reason": "stop"
				}
			],
			"usage": {
				"prompt_tokens": 1,
				"completion_tokens": 2,
				"total_tokens": 3
			}
		}
		`)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "test-key", HTTPClient: server.Client, Name: "opencode"})
	stream, err := c.CompleteStream(context.Background(), &llmtypes.Request{
		Model:    "deepseek-v4-flash",
		Messages: []llmtypes.Message{{Role: "user", Content: "ping"}},
	})
	if err != nil {
		t.Fatalf("CompleteStream returned error: %v", err)
	}
	defer stream.Close()

	var content strings.Builder
	var reasoning strings.Builder
	var finishReason string
	chunks := 0
	for {
		event, err := stream.Recv()
		if errors.Is(err, llmtypes.ErrStreamDone) {
			break
		}
		if err != nil {
			t.Fatalf("Recv() error = %v", err)
		}
		chunks++
		if len(event.Choices) > 0 {
			content.WriteString(event.Choices[0].Delta.Content)
			reasoning.WriteString(event.Choices[0].Delta.ReasoningContent)
			if event.Choices[0].FinishReason != "" {
				finishReason = event.Choices[0].FinishReason
			}
		}
	}

	if chunks != 3 {
		t.Fatalf("chunks = %d, want 3", chunks)
	}
	if content.String() != "hello" {
		t.Fatalf("content = %q, want hello", content.String())
	}
	if reasoning.String() != "r1" {
		t.Fatalf("reasoning = %q, want r1", reasoning.String())
	}
	if finishReason != "stop" {
		t.Fatalf("finishReason = %q, want stop", finishReason)
	}
}

func TestCompleteStream_StreamErrorMidFlight(t *testing.T) {
	server := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEChunk(t, w, `{"choices":[{"index":0,"delta":{"content":"a"}}]}`)
		writeSSEChunk(t, w, `{"error":{"message":"stream exploded","type":"upstream_error"}}`)
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "test-key", HTTPClient: server.Client, Name: "opencode"})
	stream, err := c.CompleteStream(context.Background(), &llmtypes.Request{
		Model:    "deepseek-v4-flash",
		Messages: []llmtypes.Message{{Role: "user", Content: "ping"}},
	})
	if err != nil {
		t.Fatalf("CompleteStream returned error: %v", err)
	}
	defer stream.Close()

	if _, err := stream.Recv(); err != nil {
		t.Fatalf("first Recv() error = %v", err)
	}
	_, err = stream.Recv()
	perr := requireProviderError(t, err)
	if perr.Kind != llmtypes.KindUpstream {
		t.Fatalf("Kind = %q, want %q", perr.Kind, llmtypes.KindUpstream)
	}
	if perr.Message != "upstream unavailable" {
		t.Fatalf("Message = %q, want sanitized upstream message", perr.Message)
	}
	if !strings.Contains(string(perr.Raw), "stream exploded") {
		t.Fatalf("Raw = %q, want original stream error preserved", string(perr.Raw))
	}
}

func TestCompleteStream_NaturalEOFWithoutDone(t *testing.T) {
	server := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEChunk(t, w, `{"choices":[{"index":0,"delta":{"content":"a"}}]}`)
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "test-key", HTTPClient: server.Client, Name: "opencode"})
	stream, err := c.CompleteStream(context.Background(), &llmtypes.Request{
		Model:    "deepseek-v4-flash",
		Messages: []llmtypes.Message{{Role: "user", Content: "ping"}},
	})
	if err != nil {
		t.Fatalf("CompleteStream returned error: %v", err)
	}
	defer stream.Close()

	if _, err := stream.Recv(); err != nil {
		t.Fatalf("first Recv() error = %v", err)
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("second Recv() error = %v, want io.EOF (lenient natural EOF)", err)
	}
}

func TestStreamSummary_Success(t *testing.T) {
	server := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEChunk(t, w, `
		{
			"id": "chat-1",
			"model": "deepseek-v4-flash",
			"choices": [
				{
					"index": 0,
					"delta": {
						"role": "assistant",
						"content": "a"
					}
				}
			]
		}
		`)
		writeSSEChunk(t, w, `{"id":"chat-1","model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"content":"b"}}]}`)
		writeSSEChunk(t, w, `
		{
			"id": "chat-1",
			"model": "deepseek-v4-flash",
			"choices": [
				{
					"index": 0,
					"delta": {},
					"finish_reason": "stop"
				}
			],
			"usage": {
				"prompt_tokens": 3,
				"completion_tokens": 2,
				"total_tokens": 5
			},
			"cost": "0.0001"
		}
		`)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "test-key", HTTPClient: server.Client, Name: "opencode"})
	stream, err := c.CompleteStream(context.Background(), &llmtypes.Request{
		Model:    "deepseek-v4-flash",
		Messages: []llmtypes.Message{{Role: "user", Content: "ping"}},
	})
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	defer stream.Close()

	for {
		if _, err := stream.Recv(); errors.Is(err, llmtypes.ErrStreamDone) {
			break
		} else if err != nil {
			t.Fatalf("Recv: %v", err)
		}
	}

	sum := stream.Summary()
	if sum == nil {
		t.Fatal("Summary returned nil")
	}
	if sum.ChunkCount != 3 {
		t.Errorf("ChunkCount = %d, want 3", sum.ChunkCount)
	}
	if sum.Model != "deepseek-v4-flash" {
		t.Errorf("Model = %q, want deepseek-v4-flash", sum.Model)
	}
	if sum.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", sum.FinishReason)
	}
	if sum.Usage == nil || sum.Usage.TotalTokens != 5 {
		t.Errorf("Usage = %+v, want TotalTokens=5", sum.Usage)
	}
	if sum.VendorCost != `"0.0001"` {
		t.Errorf("VendorCost = %q, want %q", sum.VendorCost, `"0.0001"`)
	}
	if sum.FirstByteAt.IsZero() {
		t.Error("FirstByteAt is zero, want set after first chunk")
	}
}

func TestStreamSummary_PartialOnError(t *testing.T) {
	server := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEChunk(t, w, `{"id":"x","model":"m1","choices":[{"index":0,"delta":{"content":"hi"}}]}`)
		writeSSEChunk(t, w, `{"error":{"message":"boom","type":"upstream_error"}}`)
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "test-key", HTTPClient: server.Client, Name: "opencode"})
	stream, err := c.CompleteStream(context.Background(), &llmtypes.Request{
		Model:    "deepseek-v4-flash",
		Messages: []llmtypes.Message{{Role: "user", Content: "ping"}},
	})
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	defer stream.Close()

	if _, err := stream.Recv(); err != nil {
		t.Fatalf("first Recv: %v", err)
	}
	if _, err := stream.Recv(); err == nil {
		t.Fatal("second Recv: expected error")
	}

	sum := stream.Summary()
	if sum.ChunkCount != 1 {
		t.Errorf("ChunkCount = %d, want 1 (only the pre-error chunk counts)", sum.ChunkCount)
	}
	if sum.Model != "m1" {
		t.Errorf("Model = %q, want m1", sum.Model)
	}
	if sum.FirstByteAt.IsZero() {
		t.Error("FirstByteAt is zero, want set on first chunk before failure")
	}
	if sum.FinishReason != "" {
		t.Errorf("FinishReason = %q, want empty (no finish chunk)", sum.FinishReason)
	}
	if sum.Usage != nil {
		t.Errorf("Usage = %+v, want nil (no usage in pre-error chunks)", sum.Usage)
	}
}

func writeSSEChunk(t *testing.T, w http.ResponseWriter, payload string) {
	t.Helper()
	payload = compactJSONPayload(t, payload)

	_, err := w.Write([]byte("data: " + payload + "\n\n"))
	if err != nil {
		t.Fatalf("write SSE chunk: %v", err)
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	time.Sleep(time.Millisecond)
}

func compactJSONPayload(t *testing.T, payload string) string {
	t.Helper()
	var out bytes.Buffer
	if err := json.Compact(&out, []byte(payload)); err != nil {
		t.Fatalf("compact SSE payload: %v", err)
	}
	return out.String()
}
