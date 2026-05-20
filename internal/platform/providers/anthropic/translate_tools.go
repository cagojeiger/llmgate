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

// convertOpenAITools maps the OpenAI `tools` array (each entry typed as
// {type:function, function:{name, description?, parameters?}}) into the
// Anthropic tools array. An absent or non-object `parameters` becomes an
// empty object schema so Anthropic does not reject the request.
func convertOpenAITools(raw json.RawMessage) ([]anthropicTool, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var tools []map[string]any
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, fmt.Errorf("decode tools: %w", err)
	}
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]anthropicTool, 0, len(tools))
	for _, tool := range tools {
		toolType, _ := tool["type"].(string)
		if toolType != "function" {
			return nil, fmt.Errorf("unsupported tool type %q (only 'function' is supported)", toolType)
		}
		function, ok := tool["function"].(map[string]any)
		if !ok {
			return nil, errors.New("tool.function must be an object")
		}
		name, _ := function["name"].(string)
		if strings.TrimSpace(name) == "" {
			return nil, errors.New("tool.function.name is required")
		}
		description, _ := function["description"].(string)
		var inputSchema map[string]any
		if params, has := function["parameters"]; has && params != nil {
			schema, ok := params.(map[string]any)
			if !ok {
				return nil, errors.New("tool.function.parameters must be an object")
			}
			if t, ok := schema["type"].(string); ok && t != "" && t != "object" {
				return nil, fmt.Errorf("tool.function.parameters must define an object schema, got %q", t)
			}
			inputSchema = schema
		} else {
			inputSchema = map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			}
		}
		out = append(out, anthropicTool{
			Name:        name,
			Description: description,
			InputSchema: inputSchema,
		})
	}
	return out, nil
}

// convertOpenAIToolChoice translates OpenAI tool_choice (string or
// object) into Anthropic's wire shape. The disableTools return signals
// the OpenAI "none" sentinel: Anthropic has no dedicated "none" choice,
// so the caller drops both Tools and ToolChoice from the request.
func convertOpenAIToolChoice(raw json.RawMessage) (*anthropicToolChoice, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch strings.TrimSpace(s) {
		case "", "auto":
			return &anthropicToolChoice{Type: "auto"}, false, nil
		case "required":
			return &anthropicToolChoice{Type: "any"}, false, nil
		case "none":
			return nil, true, nil
		default:
			return nil, false, fmt.Errorf("unsupported tool_choice value %q", s)
		}
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, false, fmt.Errorf("tool_choice must be a string or object: %w", err)
	}
	choiceType, _ := obj["type"].(string)
	switch choiceType {
	case "auto", "any":
		return &anthropicToolChoice{Type: choiceType}, false, nil
	case "none":
		return nil, true, nil
	case "function":
		if function, ok := obj["function"].(map[string]any); ok {
			if name, _ := function["name"].(string); strings.TrimSpace(name) != "" {
				return &anthropicToolChoice{Type: "tool", Name: name}, false, nil
			}
		}
		return nil, false, errors.New("tool_choice.function.name is required")
	case "tool":
		name, _ := obj["name"].(string)
		if name == "" {
			if function, ok := obj["function"].(map[string]any); ok {
				name, _ = function["name"].(string)
			}
		}
		if strings.TrimSpace(name) == "" {
			return nil, false, errors.New("tool_choice.name is required")
		}
		return &anthropicToolChoice{Type: "tool", Name: name}, false, nil
	default:
		return nil, false, fmt.Errorf("unsupported tool_choice type %q", choiceType)
	}
}

func parseParallelToolCalls(raw json.RawMessage) (*bool, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("parallel_tool_calls must be boolean: %w", err)
	}
	return &b, nil
}

// applyParallelToolCalls flips Anthropic's disable_parallel_tool_use bit
// when the caller explicitly sent parallel_tool_calls=false. true / nil
// leave the wire unchanged (Anthropic defaults to parallel).
func applyParallelToolCalls(choice *anthropicToolChoice, parallel *bool) *anthropicToolChoice {
	if choice == nil || parallel == nil || *parallel {
		return choice
	}
	out := *choice
	disable := true
	out.DisableParallelToolUse = &disable
	return &out
}
