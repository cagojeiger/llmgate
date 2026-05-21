package assembly

import (
	"encoding/json"
	"sort"
	"strings"
)

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
	arguments strings.Builder
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
		s.arguments.WriteString(delta.Function.Arguments)
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
			"arguments": s.arguments.String(),
		},
	}
}
