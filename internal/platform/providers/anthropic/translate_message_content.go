package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"llmgate/internal/domain/llmtypes"
)

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func anthropicMessageFromOpenAI(msg llmtypes.Message) (anthropicMessage, error) {
	content, err := buildMessageContent(msg)
	if err != nil {
		return anthropicMessage{}, err
	}
	role := msg.Role
	if role == "tool" {
		role = "user"
	}
	return anthropicMessage{Role: role, Content: content}, nil
}

func buildMessageContent(msg llmtypes.Message) (any, error) {
	if len(msg.ContentRaw) > 0 {
		return nil, errors.New("anthropic provider does not support OpenAI structured message content")
	}
	if msg.Role == "tool" {
		return buildToolResultContent(msg)
	}
	return buildAssistantContent(msg)
}

func buildToolResultContent(msg llmtypes.Message) (any, error) {
	toolCallID, err := extractStringField(msg.Extra, "tool_call_id")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(toolCallID) == "" {
		return nil, errors.New("tool message is missing tool_call_id")
	}
	return []anthropicContentBlock{{
		Type:      "tool_result",
		ToolUseID: toolCallID,
		Content:   msg.Content,
	}}, nil
}

func buildAssistantContent(msg llmtypes.Message) (any, error) {
	rawCalls, hasCalls := msg.Extra["tool_calls"]
	if !hasCalls || len(rawCalls) == 0 {
		return msg.Content, nil
	}
	var toolCalls []openAIToolCall
	if err := json.Unmarshal(rawCalls, &toolCalls); err != nil {
		return nil, fmt.Errorf("decode tool_calls: %w", err)
	}
	if len(toolCalls) == 0 {
		return msg.Content, nil
	}

	blocks := make([]anthropicContentBlock, 0, len(toolCalls)+1)
	if strings.TrimSpace(msg.Content) != "" {
		blocks = append(blocks, anthropicContentBlock{Type: "text", Text: msg.Content})
	}
	for _, tc := range toolCalls {
		block, err := toolUseBlock(tc)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, block)
	}
	return blocks, nil
}

func toolUseBlock(tc openAIToolCall) (anthropicContentBlock, error) {
	name := strings.TrimSpace(tc.Function.Name)
	if name == "" {
		return anthropicContentBlock{}, errors.New("tool_call.function.name is required")
	}
	input, err := parseToolCallArguments(tc.Function.Arguments)
	if err != nil {
		return anthropicContentBlock{}, fmt.Errorf("tool_call %q arguments: %w", name, err)
	}
	return anthropicContentBlock{
		Type:  "tool_use",
		ID:    tc.ID,
		Name:  name,
		Input: input,
	}, nil
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
