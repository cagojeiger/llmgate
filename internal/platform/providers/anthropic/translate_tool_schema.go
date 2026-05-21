package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// convertOpenAITools maps OpenAI function tools into Anthropic tool schemas.
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
		converted, err := convertOpenAIFunctionTool(tool)
		if err != nil {
			return nil, err
		}
		out = append(out, converted)
	}
	return out, nil
}

func convertOpenAIFunctionTool(tool map[string]any) (anthropicTool, error) {
	toolType, _ := tool["type"].(string)
	if toolType != "function" {
		return anthropicTool{}, fmt.Errorf("unsupported tool type %q (only 'function' is supported)", toolType)
	}
	function, ok := tool["function"].(map[string]any)
	if !ok {
		return anthropicTool{}, errors.New("tool.function must be an object")
	}
	name, _ := function["name"].(string)
	if strings.TrimSpace(name) == "" {
		return anthropicTool{}, errors.New("tool.function.name is required")
	}
	inputSchema, err := anthropicInputSchema(function)
	if err != nil {
		return anthropicTool{}, err
	}
	description, _ := function["description"].(string)
	return anthropicTool{
		Name:        name,
		Description: description,
		InputSchema: inputSchema,
	}, nil
}

func anthropicInputSchema(function map[string]any) (map[string]any, error) {
	params, has := function["parameters"]
	if !has || params == nil {
		return map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}, nil
	}
	schema, ok := params.(map[string]any)
	if !ok {
		return nil, errors.New("tool.function.parameters must be an object")
	}
	if t, ok := schema["type"].(string); ok && t != "" && t != "object" {
		return nil, fmt.Errorf("tool.function.parameters must define an object schema, got %q", t)
	}
	return schema, nil
}
