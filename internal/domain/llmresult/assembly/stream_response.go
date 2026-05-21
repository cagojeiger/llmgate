package assembly

import (
	"encoding/json"
	"sort"
	"strings"

	"llmgate/internal/domain/llmtypes"
)

// StreamResponseBuilder turns OpenAI-shaped stream chunks into the same final
// response shape a non-stream call already returns. It owns only the payload
// assembly; transport code still decides when a stream has completed.
type StreamResponseBuilder struct {
	id                string
	created           int64
	model             string
	systemFingerprint string
	usage             *llmtypes.Usage
	extra             map[string]json.RawMessage
	choices           map[int]*streamChoice
}

func NewStreamResponseBuilder() *StreamResponseBuilder {
	return &StreamResponseBuilder{choices: make(map[int]*streamChoice)}
}

func (b *StreamResponseBuilder) Add(event *llmtypes.Event) {
	if b == nil || event == nil {
		return
	}
	if event.ID != "" {
		b.id = event.ID
	}
	if event.Created != 0 {
		b.created = event.Created
	}
	if event.Model != "" {
		b.model = event.Model
	}
	if event.SystemFingerprint != "" {
		b.systemFingerprint = event.SystemFingerprint
	}
	if event.Usage != nil {
		b.usage = event.Usage.Clone()
	}
	if len(event.Extra) > 0 {
		if b.extra == nil {
			b.extra = make(map[string]json.RawMessage, len(event.Extra))
		}
		mergeRaw(b.extra, event.Extra)
	}
	for _, choice := range event.Choices {
		b.choice(choice.Index).add(choice)
	}
}

func (b *StreamResponseBuilder) Response() *llmtypes.Response {
	if b == nil {
		return nil
	}
	// b.extra was already populated by mergeRaw at Add() time, which
	// copies every RawMessage out of the caller's event. We own those
	// bytes, so the response can adopt the map directly — re-cloning at
	// terminal time would be pure work.
	resp := &llmtypes.Response{
		ID:                b.id,
		Object:            "chat.completion",
		Created:           b.created,
		Model:             b.model,
		SystemFingerprint: b.systemFingerprint,
		Usage:             b.usage.Clone(),
		Extra:             nilIfEmpty(b.extra),
	}
	indexes := make([]int, 0, len(b.choices))
	for index := range b.choices {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		resp.Choices = append(resp.Choices, b.choices[index].choice())
	}
	return resp
}

func (b *StreamResponseBuilder) choice(index int) *streamChoice {
	if b.choices == nil {
		b.choices = make(map[int]*streamChoice)
	}
	if c := b.choices[index]; c != nil {
		return c
	}
	c := &streamChoice{
		index:     index,
		msgExtra:  make(map[string]json.RawMessage),
		choiceRaw: make(map[string]json.RawMessage),
		tools:     make(map[int]*toolCallState),
	}
	b.choices[index] = c
	return c
}

type streamChoice struct {
	index            int
	role             string
	content          strings.Builder
	reasoningContent strings.Builder
	finishReason     string
	logprobs         json.RawMessage
	msgExtra         map[string]json.RawMessage
	choiceRaw        map[string]json.RawMessage
	tools            map[int]*toolCallState
}

func (c *streamChoice) add(choice llmtypes.ChoiceDelta) {
	if choice.Delta.Role != "" {
		c.role = choice.Delta.Role
	}
	c.content.WriteString(choice.Delta.Content)
	c.reasoningContent.WriteString(choice.Delta.ReasoningContent)
	if choice.FinishReason != "" {
		c.finishReason = choice.FinishReason
	}
	if len(choice.Logprobs) > 0 {
		c.logprobs = append(json.RawMessage(nil), choice.Logprobs...)
	}
	mergeRaw(c.choiceRaw, choice.Extra)
	c.addDeltaExtra(choice.Delta.Extra)
}

func (c *streamChoice) choice() llmtypes.Choice {
	c.finishToolCalls()
	// Same ownership rationale as Response().Extra above: msgExtra,
	// choiceRaw, and logprobs are slices/maps we built ourselves by
	// copying out of the per-delta event, so the terminal value adopts
	// them directly.
	return llmtypes.Choice{
		Index: c.index,
		Message: llmtypes.Message{
			Role:             c.role,
			Content:          c.content.String(),
			ReasoningContent: c.reasoningContent.String(),
			Extra:            nilIfEmpty(c.msgExtra),
		},
		FinishReason: c.finishReason,
		Logprobs:     c.logprobs,
		Extra:        nilIfEmpty(c.choiceRaw),
	}
}

func (c *streamChoice) addDeltaExtra(extra map[string]json.RawMessage) {
	if len(extra) == 0 {
		return
	}
	for key, raw := range extra {
		switch key {
		case "tool_calls":
			c.addToolCalls(raw)
		default:
			c.msgExtra[key] = append(json.RawMessage(nil), raw...)
		}
	}
}

// mergeRaw copies every src entry into dst with a fresh []byte. The
// copy is defensive: dst lives for the whole stream response, while
// src belongs to a transient event whose bytes the caller may reuse.
func mergeRaw(dst, src map[string]json.RawMessage) {
	for key, raw := range src {
		dst[key] = append(json.RawMessage(nil), raw...)
	}
}

// nilIfEmpty returns nil for empty maps so the assembled wire shape
// keeps the "no extra fields" idiom (a nil map, not an empty object)
// that downstream JSON encoders rely on.
func nilIfEmpty(m map[string]json.RawMessage) map[string]json.RawMessage {
	if len(m) == 0 {
		return nil
	}
	return m
}
