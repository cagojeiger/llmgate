// Package provider defines the single contract that every LLM upstream
// adapter implements. Adding auth, quota, or observability at the
// Provider boundary covers every gateway call site.
package provider

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
	Close() error
	// Summary returns the aggregated end-state of the stream. It is safe to
	// call at any time; the typical caller invokes it after Recv returns
	// io.EOF (or any error) so audit can extract usage / finish reason / cost
	// without re-implementing per-vendor accumulation in the handler.
	Summary() *Summary
}

// Summary captures aggregated stream state for audit purposes. Fields are
// best-effort — partial streams populate what they got, fully-failed streams
// may have everything zero. Stream/non-stream paths produce comparable shapes.
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
	// Stream is a tri-state: nil = client did not specify, *false = explicit
	// non-stream, *true = SSE. Handler dispatches to serveStream when true.
	// Adapters that always force stream (openai/stream.go) override this on
	// a copy before marshaling.
	Stream *bool `json:"stream,omitempty"`

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

// Delta is the inner object of a streaming choice — role/content/etc. arrive
// here, not on ChoiceDelta itself, matching the OpenAI chunk wire format.
type Delta struct {
	Role             string `json:"role,omitempty"`
	Content          string `json:"content,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

var ErrStreamDone = io.EOF
