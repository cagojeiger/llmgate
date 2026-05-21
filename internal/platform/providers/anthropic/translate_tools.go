package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// openAIToolCall mirrors the wire shape of one OpenAI tool_calls entry on
// an assistant message, used only for decoding what the caller forwarded
// in msg.Extra["tool_calls"].
type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func extractStringField(extra map[string]json.RawMessage, key string) (string, error) {
	raw, ok := extra[key]
	if !ok || len(raw) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("%s must be a string: %w", key, err)
	}
	return s, nil
}

// parseToolCallArguments turns OpenAI's argument string (always JSON
// serialized) back into a Go value Anthropic can re-marshal into the
// tool_use input field. Empty / whitespace-only arguments collapse to an
// empty object so the tool_use block is well-formed.
//
// Two protections beyond plain json.Unmarshal:
//
//   - UseNumber preserves large integers (e.g. unix-ms timestamps,
//     counters) which would otherwise round-trip through float64 and
//     silently lose precision when re-marshaled to Anthropic.
//   - Trailing content after the first JSON value is rejected — caller
//     sent something like `{"k":1}garbage`, almost always a serializer
//     bug on their side; failing loudly beats silently dropping the
//     trailing bytes (which is what json.Unmarshal would do).
func parseToolCallArguments(args string) (any, error) {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return map[string]any{}, nil
	}
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	decoder.UseNumber()
	var parsed any
	if err := decoder.Decode(&parsed); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return nil, errors.New("tool arguments must contain exactly one JSON object")
	} else if !errors.Is(err, io.EOF) {
		return nil, err
	}
	if _, ok := parsed.(map[string]any); !ok {
		return nil, errors.New("tool arguments must be a JSON object")
	}
	return parsed, nil
}
