// Package provider defines the single contract that every LLM upstream
// adapter implements. Both the HTTP gateway (cmd/llmgate) and the
// diagnostic CLI (cmd/llmgate-probe) consume this — adding auth, quota,
// or observability at the Provider boundary covers both call sites.
package provider

import (
	"context"
	"encoding/json"
	"io"
)

type Provider interface {
	Name() string
	Complete(ctx context.Context, req *Request) (*Response, error)
	CompleteStream(ctx context.Context, req *Request) (Stream, error)
}

type Stream interface {
	Recv() (*Event, error)
	Close() error
}

type Request struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`

	MaxTokens   int      `json:"max_tokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	Stop        []string `json:"stop,omitempty"`
	Seed        *int     `json:"seed,omitempty"`
	User        string   `json:"user,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

func (r *Request) Validate() error {
	if r == nil {
		return &Error{Kind: KindBadRequest, Message: "request is nil"}
	}
	if r.Model == "" {
		return &Error{Kind: KindBadRequest, Message: "model is required"}
	}
	if len(r.Messages) == 0 {
		return &Error{Kind: KindBadRequest, Message: "messages must not be empty"}
	}
	return nil
}

type Message struct {
	Role             string `json:"role"`
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

type Response struct {
	ID                string `json:"id"`
	Object            string `json:"object,omitempty"`
	Created           int64  `json:"created,omitempty"`
	Model             string `json:"model"`
	SystemFingerprint string `json:"system_fingerprint,omitempty"`

	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

type Choice struct {
	Index        int             `json:"index"`
	Message      Message         `json:"message"`
	FinishReason string          `json:"finish_reason"`
	Logprobs     json.RawMessage `json:"logprobs,omitempty"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`

	Extra map[string]json.RawMessage `json:"-"`
}

type Event struct {
	ID                string `json:"id,omitempty"`
	Object            string `json:"object,omitempty"`
	Created           int64  `json:"created,omitempty"`
	Model             string `json:"model,omitempty"`
	SystemFingerprint string `json:"system_fingerprint,omitempty"`

	Choices []ChoiceDelta `json:"choices,omitempty"`
	Usage   *Usage        `json:"usage,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

type ChoiceDelta struct {
	Index            int             `json:"index"`
	Role             string          `json:"role,omitempty"`
	Content          string          `json:"content,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	FinishReason     string          `json:"finish_reason,omitempty"`
	Logprobs         json.RawMessage `json:"logprobs,omitempty"`
}

var ErrStreamDone = io.EOF
