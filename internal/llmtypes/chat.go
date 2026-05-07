// Package llmtypes defines the shared gateway contract and OpenAI-shaped wire types.
package llmtypes

import (
	"context"
	"encoding/json"
	"io"
	"time"
)

type Provider interface {
	Name() string
	Complete(ctx context.Context, req *Request) (*Response, error)
	CompleteStream(ctx context.Context, req *Request) (Stream, error)
}

type Stream interface {
	Recv() (*Event, error)
	// Close must be safe to call while Recv is blocked, and must cause any
	// in-flight Recv to return promptly (within seconds, not minutes) — the
	// Service/handler use Close to break out of pending reads when the
	// request times out or the client disconnects. Callers apply a bounded
	// grace period after Close as a safety net; if Recv still does not
	// return, the spawning goroutine is abandoned (Go cannot forcibly
	// reclaim it) and the underlying body may stay open longer than
	// expected.
	Close() error
	// Summary returns best-effort stream totals for audit.
	Summary() *Summary
}

// Summary captures best-effort stream state for audit.
type Summary struct {
	Model        string
	FinishReason string
	Usage        *Usage
	VendorCost   string
	ChunkCount   int
	FirstByteAt  time.Time
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
	// Stream is tri-state: nil = omitted, false = non-stream, true = SSE.
	Stream *bool `json:"stream,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

func (r *Request) Validate() error {
	if r == nil {
		return &Error{ErrorKind: KindBadRequest, Message: "request is nil"}
	}
	if r.Model == "" {
		return &Error{ErrorKind: KindBadRequest, Message: "model is required"}
	}
	if len(r.Messages) == 0 {
		return &Error{ErrorKind: KindBadRequest, Message: "messages must not be empty"}
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

	Extra map[string]json.RawMessage `json:"-"`
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
	Index        int             `json:"index"`
	Delta        Delta           `json:"delta"`
	FinishReason string          `json:"finish_reason,omitempty"`
	Logprobs     json.RawMessage `json:"logprobs,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Delta is the inner object of a streaming choice.
type Delta struct {
	Role             string `json:"role,omitempty"`
	Content          string `json:"content,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

var ErrStreamDone = io.EOF
