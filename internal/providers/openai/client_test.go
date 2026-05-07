package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"llmgate/internal/llmtypes"
)

func TestComplete_Success(t *testing.T) {
	server := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %s, want /chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want Bearer test-key", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q, want application/json", got)
		}
		if got := r.Header.Get("User-Agent"); got != defaultUserAgent {
			t.Errorf("User-Agent = %q, want %q", got, defaultUserAgent)
		}

		body, _ := io.ReadAll(r.Body)
		var got llmtypes.Request
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}
		if got.Model != "deepseek-v4-flash" {
			t.Errorf("model = %q, want deepseek-v4-flash", got.Model)
		}
		if len(got.Messages) != 1 || got.Messages[0].Content != "ping" {
			t.Errorf("messages = %+v, want [{user,ping}]", got.Messages)
		}
		if string(got.Extra["vendor_request"]) != `"keep"` {
			t.Errorf("vendor_request extra = %s, want keep", got.Extra["vendor_request"])
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "chat-1",
			"object": "chat.completion",
			"model": "deepseek-v4-flash",
			"choices": [{
				"index": 0,
				"message": {"role": "assistant", "content": "pong", "reasoning_content": "because", "vendor_msg": 1},
				"finish_reason": "stop"
			}],
			"usage": {"prompt_tokens": 5, "completion_tokens": 1, "total_tokens": 6, "prompt_cache_hit_tokens": 4},
			"cost": 0.001
		}`))
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "test-key", HTTPClient: server.Client, Name: "opencode"})
	resp, err := c.Complete(context.Background(), &llmtypes.Request{
		Model:     "deepseek-v4-flash",
		Messages:  []llmtypes.Message{{Role: "user", Content: "ping"}},
		MaxTokens: 32,
		Extra:     map[string]json.RawMessage{"vendor_request": json.RawMessage(`"keep"`)},
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	if resp.ID != "chat-1" {
		t.Errorf("ID = %q, want chat-1", resp.ID)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("len(Choices) = %d, want 1", len(resp.Choices))
	}
	if resp.Choices[0].Message.Content != "pong" {
		t.Errorf("content = %q, want pong", resp.Choices[0].Message.Content)
	}
	if resp.Choices[0].Message.ReasoningContent != "because" {
		t.Errorf("reasoning_content = %q, want because", resp.Choices[0].Message.ReasoningContent)
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 6 {
		t.Errorf("usage = %+v, want TotalTokens=6", resp.Usage)
	}
	if string(resp.Extra["cost"]) != "0.001" {
		t.Errorf("cost extra = %s, want 0.001", resp.Extra["cost"])
	}
	if string(resp.Usage.Extra["prompt_cache_hit_tokens"]) != "4" {
		t.Errorf("usage extra = %s, want 4", resp.Usage.Extra["prompt_cache_hit_tokens"])
	}
}

func TestComplete_UpstreamErrorEnvelope(t *testing.T) {
	server := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key","type":"authentication_error"}}`))
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "bad-key", HTTPClient: server.Client, Name: "opencode"})
	_, err := c.Complete(context.Background(), &llmtypes.Request{
		Model:    "deepseek-v4-flash",
		Messages: []llmtypes.Message{{Role: "user", Content: "ping"}},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	perr := requireProviderError(t, err)
	if perr.ErrorKind != llmtypes.KindAuth {
		t.Errorf("ErrorKind = %q, want %q", perr.ErrorKind, llmtypes.KindAuth)
	}
	if !strings.Contains(perr.Message, "invalid api key") {
		t.Errorf("Message = %q, want substring 'invalid api key'", perr.Message)
	}
	if perr.Provider != "opencode" {
		t.Errorf("Provider = %q, want opencode", perr.Provider)
	}
	if perr.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want 401", perr.StatusCode)
	}
}

func TestComplete_UpstreamErrorNonJSON(t *testing.T) {
	server := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream gateway down"))
	}))
	defer server.Close()

	c := mustNew(t, Config{BaseURL: server.URL, APIKey: "k", HTTPClient: server.Client, Name: "opencode"})
	_, err := c.Complete(context.Background(), &llmtypes.Request{
		Model:    "deepseek-v4-flash",
		Messages: []llmtypes.Message{{Role: "user", Content: "ping"}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	perr := requireProviderError(t, err)
	if perr.ErrorKind != llmtypes.KindUpstream {
		t.Errorf("ErrorKind = %q, want %q", perr.ErrorKind, llmtypes.KindUpstream)
	}
	if !strings.Contains(perr.Message, "upstream gateway down") {
		t.Errorf("Message = %q, want substring 'upstream gateway down'", perr.Message)
	}
}

func TestComplete_ValidationErrors(t *testing.T) {
	c := mustNew(t, Config{BaseURL: "http://example.invalid", APIKey: "k", Name: "opencode"})
	cases := []struct {
		name string
		req  *llmtypes.Request
	}{
		{"nil", nil},
		{"empty model", &llmtypes.Request{Messages: []llmtypes.Message{{Role: "user", Content: "x"}}}},
		{"empty messages", &llmtypes.Request{Model: "m"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Complete(context.Background(), tc.req)
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			perr := requireProviderError(t, err)
			if perr.ErrorKind != llmtypes.KindBadRequest {
				t.Fatalf("ErrorKind = %q, want %q", perr.ErrorKind, llmtypes.KindBadRequest)
			}
		})
	}
}

// TestClassify_StatusAndEnvelope drives the classify helper directly so we
// can pin every HTTP-status / envelope-shape mapping in one place. New
// cases should land here before being touched in other tests.
func TestClassify_StatusAndEnvelope(t *testing.T) {
	c := mustNew(t, Config{BaseURL: "http://example.invalid", APIKey: "k", Name: "opencode"})
	cases := []struct {
		name   string
		status int
		body   string
		want   llmtypes.ErrorKind
	}{
		{"401 auth", 401, `{"error":{"message":"bad key","type":"authentication_error"}}`, llmtypes.KindAuth},
		{"403 auth", 403, `{"error":{"message":"forbidden"}}`, llmtypes.KindAuth},
		{"404 maps to bad_request", 404, `{"error":{"message":"no such model"}}`, llmtypes.KindBadRequest},
		{"408 request timeout", 408, `{"error":{"message":"server timeout"}}`, llmtypes.KindTimeout},
		{"413 request too large", 413, `{"error":{"message":"payload too large","type":"request_too_large"}}`, llmtypes.KindBadRequest},
		{"413 with token-limit hint becomes context_length", 413, `{"error":{"message":"prompt exceeded token limit"}}`, llmtypes.KindContextLength},
		{"422 unprocessable", 422, `{"error":{"message":"bad fields"}}`, llmtypes.KindBadRequest},
		{"400 with context_length hint", 400, `{"error":{"message":"context length 8000 exceeded"}}`, llmtypes.KindContextLength},
		{"400 content_filter via type", 400, `{"error":{"message":"blocked","type":"content_filter"}}`, llmtypes.KindContentFilter},
		{"400 content_filter via code", 400, `{"error":{"message":"blocked","type":"invalid_request_error","code":"content_filter"}}`, llmtypes.KindContentFilter},
		{"429 rate limit", 429, `{"error":{"message":"slow down"}}`, llmtypes.KindRateLimit},
		{"500 upstream", 500, `{"error":{"message":"internal"}}`, llmtypes.KindUpstream},
		{"529 overload (Anthropic-style status some gateways forward)", 529, `{"error":{"message":"overloaded"}}`, llmtypes.KindUpstream},
		{"non-string code does not break parsing", 400, `{"error":{"message":"bad","code":123}}`, llmtypes.KindBadRequest},
		{"unparseable body falls to status mapping", 502, `<html>oops</html>`, llmtypes.KindUpstream},
		{"unmapped 4xx remains unknown", 451, `{"error":{"message":"legal hold"}}`, llmtypes.KindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			perr := c.classify(tc.status, []byte(tc.body), "")
			if perr.ErrorKind != tc.want {
				t.Errorf("ErrorKind = %q, want %q (body=%s)", perr.ErrorKind, tc.want, tc.body)
			}
			if perr.StatusCode != tc.status {
				t.Errorf("StatusCode = %d, want %d (preserved verbatim)", perr.StatusCode, tc.status)
			}
		})
	}
}

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
		writeSSEChunk(t, w, `{"id":"chat-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
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
	if perr.ErrorKind != llmtypes.KindUpstream {
		t.Fatalf("ErrorKind = %q, want %q", perr.ErrorKind, llmtypes.KindUpstream)
	}
	if !strings.Contains(perr.Message, "stream exploded") {
		t.Fatalf("Message = %q, want stream exploded", perr.Message)
	}
}

func TestParseStreamError_UsesOpenAIKindClassifier(t *testing.T) {
	cases := []struct {
		name string
		body string
		want llmtypes.ErrorKind
	}{
		{"auth type", `{"error":{"message":"bad key","type":"authentication_error"}}`, llmtypes.KindAuth},
		{"rate type", `{"error":{"message":"slow","type":"rate_limit_error"}}`, llmtypes.KindRateLimit},
		{"context code", `{"error":{"message":"too long","type":"invalid_request_error","code":"context_length_exceeded"}}`, llmtypes.KindContextLength},
		{"content filter code", `{"error":{"message":"blocked","type":"invalid_request_error","code":"content_filter"}}`, llmtypes.KindContentFilter},
		{"invalid type", `{"error":{"message":"bad field","type":"invalid_request_error"}}`, llmtypes.KindBadRequest},
		{"unknown stream envelope", `{"error":{"message":"boom","type":"future_unknown"}}`, llmtypes.KindUpstream},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			perr := parseStreamError([]byte(tc.body), "opencode")
			if perr == nil {
				t.Fatal("parseStreamError returned nil")
			}
			if perr.ErrorKind != tc.want {
				t.Fatalf("ErrorKind = %q, want %q", perr.ErrorKind, tc.want)
			}
		})
	}
}

// TestCompleteStream_NaturalEOFWithoutDone exercises the lenient
// terminator policy: an upstream that delivers events but ends the
// stream without the OpenAI `[DONE]` sentinel produces a clean io.EOF
// rather than a synthesized "missing [DONE]" error. This keeps the
// SSE reader interoperable with vendors (Anthropic) that don't emit
// `[DONE]` at all.
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
		writeSSEChunk(t, w, `{"id":"chat-1","model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"role":"assistant","content":"a"}}]}`)
		writeSSEChunk(t, w, `{"id":"chat-1","model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"content":"b"}}]}`)
		writeSSEChunk(t, w, `{"id":"chat-1","model":"deepseek-v4-flash","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5},"cost":"0.0001"}`)
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

	_, err := w.Write([]byte("data: " + payload + "\n\n"))
	if err != nil {
		t.Fatalf("write SSE chunk: %v", err)
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	time.Sleep(time.Millisecond)
}

func requireProviderError(t *testing.T, err error) *llmtypes.Error {
	t.Helper()

	var perr *llmtypes.Error
	if !errors.As(err, &perr) {
		t.Fatalf("err type = %T, want *llmtypes.Error", err)
	}
	return perr
}

func mustNew(t *testing.T, cfg Config) *Client {
	t.Helper()

	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return c
}

type localServer struct {
	*httptest.Server
	Client *http.Client
}

func newLocalServer(handler http.Handler) *localServer {
	listener := newPipeListener()
	server := &httptest.Server{
		Listener: listener,
		Config:   &http.Server{Handler: handler},
	}
	server.Start()

	transport := &http.Transport{DialContext: listener.DialContext}
	return &localServer{
		Server: server,
		Client: &http.Client{Transport: transport},
	}
}

func (s *localServer) Close() {
	if transport, ok := s.Client.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
	s.Server.Close()
}

type pipeListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{
		conns:  make(chan net.Conn),
		closed: make(chan struct{}),
	}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() {
		close(l.closed)
	})
	return nil
}

func (l *pipeListener) Addr() net.Addr {
	return pipeAddr("pipe")
}

func (l *pipeListener) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	clientConn, serverConn := net.Pipe()
	select {
	case l.conns <- serverConn:
		return clientConn, nil
	case <-ctx.Done():
		_ = clientConn.Close()
		_ = serverConn.Close()
		return nil, ctx.Err()
	case <-l.closed:
		_ = clientConn.Close()
		_ = serverConn.Close()
		return nil, net.ErrClosed
	}
}

type pipeAddr string

func (a pipeAddr) Network() string { return "pipe" }
func (a pipeAddr) String() string  { return string(a) }
