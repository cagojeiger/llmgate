package provider

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"llmgate/internal/catalog"
)

func TestRouter_OpenAIOnly(t *testing.T) {
	var logs bytes.Buffer
	openAI := &fakeProvider{name: "openai"}

	router, err := NewRouter(stubCatalog(), map[string]AdapterFactory{
		"openai": func(ep *catalog.Endpoint) (Provider, error) {
			return openAI, nil
		},
	}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}
	if got := len(router.byModel); got != 12 {
		t.Fatalf("len(byModel) = %d, want 12", got)
	}
	if _, ok := router.byModel["minimax-m2.7"]; ok {
		t.Fatalf("anthropic model registered without anthropic factory")
	}
	if !strings.Contains(logs.String(), "no adapter for protocol") {
		t.Fatalf("logs = %q, want missing protocol warning", logs.String())
	}
}

func TestRouter_Both(t *testing.T) {
	router, err := NewRouter(stubCatalog(), map[string]AdapterFactory{
		"openai": func(ep *catalog.Endpoint) (Provider, error) {
			return &fakeProvider{name: "openai"}, nil
		},
		"anthropic": func(ep *catalog.Endpoint) (Provider, error) {
			return &fakeProvider{name: "anthropic"}, nil
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}
	if got := len(router.byModel); got != 14 {
		t.Fatalf("len(byModel) = %d, want 14", got)
	}
}

func TestRouter_UnknownModel(t *testing.T) {
	router, err := NewRouter(stubCatalog(), map[string]AdapterFactory{
		"openai": func(ep *catalog.Endpoint) (Provider, error) {
			return &fakeProvider{name: "openai"}, nil
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}

	_, err = router.Complete(context.Background(), &Request{
		Model:    "nonexistent-model-123",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var perr *Error
	if !errors.As(err, &perr) {
		t.Fatalf("err type = %T, want *Error", err)
	}
	if perr.Kind != KindBadRequest {
		t.Fatalf("Kind = %q, want %q", perr.Kind, KindBadRequest)
	}
}

func TestRouter_Dispatch(t *testing.T) {
	openAI := &fakeProvider{name: "openai"}
	anthropic := &fakeProvider{name: "anthropic"}
	router, err := NewRouter(stubCatalog(), map[string]AdapterFactory{
		"openai": func(ep *catalog.Endpoint) (Provider, error) {
			return openAI, nil
		},
		"anthropic": func(ep *catalog.Endpoint) (Provider, error) {
			return anthropic, nil
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}

	req := &Request{Model: "kimi-k2.6", Messages: []Message{{Role: "user", Content: "hi"}}}
	if _, err := router.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if openAI.completeCalls != 1 || openAI.lastCompleteReq != req {
		t.Fatalf("openai Complete calls = %d, last req = %p, want %p", openAI.completeCalls, openAI.lastCompleteReq, req)
	}
	if anthropic.completeCalls != 0 {
		t.Fatalf("anthropic Complete calls = %d, want 0", anthropic.completeCalls)
	}

	streamReq := &Request{Model: "minimax-m2.5", Messages: []Message{{Role: "user", Content: "hi"}}}
	if _, err := router.CompleteStream(context.Background(), streamReq); err != nil {
		t.Fatalf("CompleteStream() error = %v", err)
	}
	if anthropic.streamCalls != 1 || anthropic.lastStreamReq != streamReq {
		t.Fatalf("anthropic stream calls = %d, last req = %p, want %p", anthropic.streamCalls, anthropic.lastStreamReq, streamReq)
	}
}

func stubCatalog() *catalog.Catalog {
	endpoints := map[string]*catalog.Endpoint{
		"opencode-openai": {
			Name:       "opencode-openai",
			Vendor:     "opencode",
			BaseURL:    "http://example.test/openai",
			APIKey:     "key",
			Protocol:   "openai",
			AuthScheme: "bearer",
		},
		"opencode-anthropic": {
			Name:       "opencode-anthropic",
			Vendor:     "opencode",
			BaseURL:    "http://example.test/anthropic",
			APIKey:     "key",
			Protocol:   "anthropic",
			AuthScheme: "x-api-key",
		},
	}
	models := make(map[string]*catalog.Model)
	for _, id := range []string{
		"glm-5.1",
		"glm-5",
		"kimi-k2.5",
		"kimi-k2.6",
		"deepseek-v4-pro",
		"deepseek-v4-flash",
		"mimo-v2-pro",
		"mimo-v2-omni",
		"mimo-v2.5-pro",
		"mimo-v2.5",
		"qwen3.6-plus",
		"qwen3.5-plus",
	} {
		models[id] = &catalog.Model{ID: id, Endpoint: "opencode-openai"}
	}
	for _, id := range []string{"minimax-m2.7", "minimax-m2.5"} {
		models[id] = &catalog.Model{ID: id, Endpoint: "opencode-anthropic"}
	}
	return &catalog.Catalog{
		Endpoints: endpoints,
		Models:    models,
		Defaults:  catalog.Defaults{Model: "deepseek-v4-flash"},
	}
}

type fakeProvider struct {
	name            string
	completeCalls   int
	streamCalls     int
	lastCompleteReq *Request
	lastStreamReq   *Request
}

func (p *fakeProvider) Name() string { return p.name }

func (p *fakeProvider) Complete(ctx context.Context, req *Request) (*Response, error) {
	p.completeCalls++
	p.lastCompleteReq = req
	return &Response{Model: req.Model, Choices: []Choice{{Index: 0, Message: Message{Role: "assistant", Content: "ok"}}}}, nil
}

func (p *fakeProvider) CompleteStream(ctx context.Context, req *Request) (Stream, error) {
	p.streamCalls++
	p.lastStreamReq = req
	return fakeStream{}, nil
}

type fakeStream struct{}

func (fakeStream) Recv() (*Event, error) { return nil, ErrStreamDone }
func (fakeStream) Close() error          { return nil }
func (fakeStream) Summary() *Summary     { return &Summary{} }
