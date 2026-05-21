package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// convertOpenAIToolChoice translates OpenAI tool_choice into Anthropic's shape.
// The disableTools return handles OpenAI's "none" sentinel by dropping tools.
func convertOpenAIToolChoice(raw json.RawMessage) (*anthropicToolChoice, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return convertToolChoiceString(s)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, false, fmt.Errorf("tool_choice must be a string or object: %w", err)
	}
	return convertToolChoiceObject(obj)
}

func convertToolChoiceString(s string) (*anthropicToolChoice, bool, error) {
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

func convertToolChoiceObject(obj map[string]any) (*anthropicToolChoice, bool, error) {
	choiceType, _ := obj["type"].(string)
	switch choiceType {
	case "auto", "any":
		return &anthropicToolChoice{Type: choiceType}, false, nil
	case "none":
		return nil, true, nil
	case "function":
		name := functionChoiceName(obj)
		if strings.TrimSpace(name) == "" {
			return nil, false, errors.New("tool_choice.function.name is required")
		}
		return &anthropicToolChoice{Type: "tool", Name: name}, false, nil
	case "tool":
		name := toolChoiceName(obj)
		if strings.TrimSpace(name) == "" {
			return nil, false, errors.New("tool_choice.name is required")
		}
		return &anthropicToolChoice{Type: "tool", Name: name}, false, nil
	default:
		return nil, false, fmt.Errorf("unsupported tool_choice type %q", choiceType)
	}
}

func functionChoiceName(obj map[string]any) string {
	function, ok := obj["function"].(map[string]any)
	if !ok {
		return ""
	}
	name, _ := function["name"].(string)
	return name
}

func toolChoiceName(obj map[string]any) string {
	name, _ := obj["name"].(string)
	if name != "" {
		return name
	}
	return functionChoiceName(obj)
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
// when the caller explicitly sent parallel_tool_calls=false.
func applyParallelToolCalls(choice *anthropicToolChoice, parallel *bool) *anthropicToolChoice {
	if choice == nil || parallel == nil || *parallel {
		return choice
	}
	out := *choice
	disable := true
	out.DisableParallelToolUse = &disable
	return &out
}
