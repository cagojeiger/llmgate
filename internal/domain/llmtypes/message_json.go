package llmtypes

import (
	"bytes"
	"encoding/json"
)

func (m *Message) UnmarshalJSON(b []byte) error {
	type wire struct {
		Role             string          `json:"role"`
		Content          json.RawMessage `json:"content"`
		ReasoningContent string          `json:"reasoning_content,omitempty"`
		Extra            map[string]json.RawMessage
	}
	var w wire
	if err := unmarshalWithExtra(b, &w, messageJSONFields); err != nil {
		return err
	}

	*m = Message{
		Role:             w.Role,
		ReasoningContent: w.ReasoningContent,
		Extra:            w.Extra,
	}
	if len(w.Content) == 0 {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(w.Content), []byte("null")) {
		m.ContentRaw = append(json.RawMessage(nil), w.Content...)
		return nil
	}
	var text string
	if err := json.Unmarshal(w.Content, &text); err == nil {
		m.Content = text
		return nil
	}
	m.ContentRaw = append(json.RawMessage(nil), w.Content...)
	return nil
}

func (m Message) MarshalJSON() ([]byte, error) {
	type wire struct {
		Role             string `json:"role"`
		Content          string `json:"content"`
		ReasoningContent string `json:"reasoning_content,omitempty"`
	}
	base, err := marshalWithExtra(wire{
		Role:             m.Role,
		Content:          m.Content,
		ReasoningContent: m.ReasoningContent,
	}, m.Extra)
	if err != nil {
		return nil, err
	}
	if len(m.ContentRaw) == 0 {
		return base, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(base, &raw); err != nil {
		return nil, err
	}
	raw["content"] = append(json.RawMessage(nil), m.ContentRaw...)
	return json.Marshal(raw)
}
