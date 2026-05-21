package assembly

import (
	"encoding/json"
	"sort"

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
		b.usage = cloneUsage(event.Usage)
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
	resp := &llmtypes.Response{
		ID:                b.id,
		Object:            "chat.completion",
		Created:           b.created,
		Model:             b.model,
		SystemFingerprint: b.systemFingerprint,
		Usage:             cloneUsage(b.usage),
		Extra:             cloneRawMap(b.extra),
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
	content          string
	reasoningContent string
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
	c.content += choice.Delta.Content
	c.reasoningContent += choice.Delta.ReasoningContent
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
	return llmtypes.Choice{
		Index: c.index,
		Message: llmtypes.Message{
			Role:             c.role,
			Content:          c.content,
			ReasoningContent: c.reasoningContent,
			Extra:            cloneRawMap(c.msgExtra),
		},
		FinishReason: c.finishReason,
		Logprobs:     append(json.RawMessage(nil), c.logprobs...),
		Extra:        cloneRawMap(c.choiceRaw),
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

func (c *streamChoice) addToolCalls(raw json.RawMessage) {
	var deltas []toolCallDelta
	if err := json.Unmarshal(raw, &deltas); err != nil {
		c.msgExtra["tool_calls"] = append(json.RawMessage(nil), raw...)
		return
	}
	for pos, delta := range deltas {
		index := pos
		if delta.Index != nil {
			index = *delta.Index
		}
		state := c.tools[index]
		if state == nil {
			state = &toolCallState{Index: index}
			c.tools[index] = state
		}
		state.add(delta)
	}
}

func (c *streamChoice) finishToolCalls() {
	if len(c.tools) == 0 {
		return
	}
	indexes := make([]int, 0, len(c.tools))
	for index := range c.tools {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	out := make([]map[string]any, 0, len(indexes))
	for _, index := range indexes {
		out = append(out, c.tools[index].wire())
	}
	raw, err := json.Marshal(out)
	if err == nil {
		c.msgExtra["tool_calls"] = raw
	}
}

type toolCallDelta struct {
	Index    *int              `json:"index,omitempty"`
	ID       string            `json:"id,omitempty"`
	Type     string            `json:"type,omitempty"`
	Function *toolCallFunction `json:"function,omitempty"`
}

type toolCallFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type toolCallState struct {
	Index     int
	ID        string
	Type      string
	Name      string
	Arguments string
}

func (s *toolCallState) add(delta toolCallDelta) {
	if delta.ID != "" {
		s.ID = delta.ID
	}
	if delta.Type != "" {
		s.Type = delta.Type
	}
	if delta.Function != nil {
		if delta.Function.Name != "" {
			s.Name = delta.Function.Name
		}
		s.Arguments += delta.Function.Arguments
	}
}

func (s *toolCallState) wire() map[string]any {
	typ := s.Type
	if typ == "" {
		typ = "function"
	}
	return map[string]any{
		"id":   s.ID,
		"type": typ,
		"function": map[string]any{
			"name":      s.Name,
			"arguments": s.Arguments,
		},
	}
}

func mergeRaw(dst, src map[string]json.RawMessage) {
	for key, raw := range src {
		dst[key] = append(json.RawMessage(nil), raw...)
	}
}

func cloneRawMap(in map[string]json.RawMessage) map[string]json.RawMessage {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]json.RawMessage, len(in))
	mergeRaw(out, in)
	return out
}

func cloneUsage(usage *llmtypes.Usage) *llmtypes.Usage {
	if usage == nil {
		return nil
	}
	out, err := json.Marshal(usage)
	if err != nil {
		return nil
	}
	var cloned llmtypes.Usage
	if err := json.Unmarshal(out, &cloned); err != nil {
		return nil
	}
	return &cloned
}
