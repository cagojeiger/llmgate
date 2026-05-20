package server

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"llmgate/internal/llmtypes"
)

type streamCapture struct {
	events  []json.RawMessage
	top     map[string]any
	choices map[int]*streamChoiceCapture
	usage   *llmtypes.Usage
}

type streamChoiceCapture struct {
	index     int
	message   map[string]json.RawMessage
	content   strings.Builder
	reasoning strings.Builder
	finish    string
	logprobs  json.RawMessage
}

func newStreamCapture() *streamCapture {
	return &streamCapture{
		top:     make(map[string]any),
		choices: make(map[int]*streamChoiceCapture),
	}
}

func (c *streamCapture) Add(payload json.RawMessage, event *llmtypes.Event) {
	if c == nil {
		return
	}
	if payload != nil {
		c.events = append(c.events, append(json.RawMessage(nil), payload...))
	}
	if event == nil {
		return
	}
	if event.ID != "" {
		c.top["id"] = event.ID
	}
	if event.Created != 0 {
		c.top["created"] = event.Created
	}
	if event.Model != "" {
		c.top["model"] = event.Model
	}
	if event.SystemFingerprint != "" {
		c.top["system_fingerprint"] = event.SystemFingerprint
	}
	if event.Usage != nil {
		c.usage = event.Usage
	}
	for _, choice := range event.Choices {
		cc := c.choice(choice.Index)
		if choice.FinishReason != "" {
			cc.finish = choice.FinishReason
		}
		if len(choice.Logprobs) > 0 {
			cc.logprobs = append(json.RawMessage(nil), choice.Logprobs...)
		}
		cc.mergeDelta(choice.Delta)
	}
}

func (c *streamCapture) Events() []json.RawMessage {
	if c == nil || len(c.events) == 0 {
		return nil
	}
	out := make([]json.RawMessage, len(c.events))
	for i := range c.events {
		out[i] = append(json.RawMessage(nil), c.events[i]...)
	}
	return out
}

func (c *streamCapture) BuildResponse(summary *llmtypes.Summary) (json.RawMessage, error) {
	if c == nil {
		return nil, nil
	}
	raw := make(map[string]any, len(c.top)+3)
	for k, v := range c.top {
		raw[k] = v
	}
	raw["object"] = "chat.completion"
	if summary != nil {
		if summary.Model != "" {
			raw["model"] = summary.Model
		}
		if summary.Usage != nil {
			c.usage = summary.Usage
		}
	}
	indexes := make([]int, 0, len(c.choices))
	for idx := range c.choices {
		indexes = append(indexes, idx)
	}
	sort.Ints(indexes)
	choices := make([]map[string]any, 0, len(indexes))
	for _, idx := range indexes {
		choices = append(choices, c.choices[idx].toResponseChoice())
	}
	raw["choices"] = choices
	if c.usage != nil {
		raw["usage"] = c.usage
	}
	return json.Marshal(raw)
}

func (c *streamCapture) choice(index int) *streamChoiceCapture {
	if got := c.choices[index]; got != nil {
		return got
	}
	cc := &streamChoiceCapture{
		index:   index,
		message: map[string]json.RawMessage{"role": json.RawMessage(`"assistant"`)},
	}
	c.choices[index] = cc
	return cc
}

func (c *streamChoiceCapture) mergeDelta(delta llmtypes.Delta) {
	raw, err := json.Marshal(delta)
	if err != nil {
		return
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return
	}
	for key, value := range fields {
		if isJSONNullOrEmpty(value) {
			continue
		}
		if key == "content" {
			appendJSONString(&c.content, value)
			continue
		}
		if key == "reasoning_content" || key == "reasoning" {
			appendJSONString(&c.reasoning, value)
			continue
		}
		c.message[key] = append(json.RawMessage(nil), value...)
	}
}

func (c *streamChoiceCapture) toResponseChoice() map[string]any {
	message := make(map[string]json.RawMessage, len(c.message)+2)
	for k, v := range c.message {
		message[k] = append(json.RawMessage(nil), v...)
	}
	if c.content.Len() > 0 {
		message["content"] = marshalString(c.content.String())
	}
	if c.reasoning.Len() > 0 {
		message["reasoning_content"] = marshalString(c.reasoning.String())
	}
	choice := map[string]any{
		"index":         c.index,
		"message":       message,
		"finish_reason": c.finish,
	}
	if len(c.logprobs) > 0 {
		choice["logprobs"] = c.logprobs
	}
	return choice
}

func appendJSONString(dst *strings.Builder, raw json.RawMessage) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		dst.WriteString(s)
	}
}

func marshalString(s string) json.RawMessage {
	return json.RawMessage(strconv.Quote(s))
}

func isJSONNullOrEmpty(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed == "" || trimmed == "null" || trimmed == `""`
}
