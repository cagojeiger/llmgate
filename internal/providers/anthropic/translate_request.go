package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"llmgate/internal/llmtypes"
)

func toAnthropicRequest(req *llmtypes.Request, defaultMaxTokens int, stream bool) ([]byte, error) {
	var system []string
	messages := make([]anthropicMessage, 0, len(req.Messages))
	for _, msg := range req.Messages {
		if msg.Role == "system" {
			if len(msg.ContentRaw) > 0 {
				return nil, errors.New("anthropic provider does not support structured system content")
			}
			system = append(system, msg.Content)
			continue
		}
		content, err := buildMessageContent(msg)
		if err != nil {
			return nil, err
		}
		role := msg.Role
		if role == "tool" {
			// OpenAI uses role=tool for tool result messages; Anthropic
			// represents the same turn as a user message containing a
			// tool_result block.
			role = "user"
		}
		messages = append(messages, anthropicMessage{
			Role:    role,
			Content: content,
		})
	}
	if len(messages) == 0 {
		return nil, errors.New("messages must include at least one non-system message")
	}

	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}
	out := anthropicRequest{
		Model:         req.Model,
		Messages:      messages,
		System:        strings.Join(system, "\n\n"),
		MaxTokens:     maxTokens,
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		StopSequences: req.Stop,
		Stream:        stream,
	}

	// Tool support: read OpenAI-shaped fields from Extra, translate, and
	// record them as consumed so the trailing Extra-merge does not leak
	// the OpenAI shape back onto the wire.
	consumed := map[string]struct{}{}
	if raw, ok := req.Extra["tools"]; ok {
		tools, err := convertOpenAITools(raw)
		if err != nil {
			return nil, err
		}
		out.Tools = tools
		consumed["tools"] = struct{}{}
	}
	var disableTools bool
	if raw, ok := req.Extra["tool_choice"]; ok {
		choice, disable, err := convertOpenAIToolChoice(raw)
		if err != nil {
			return nil, err
		}
		out.ToolChoice = choice
		disableTools = disable
		consumed["tool_choice"] = struct{}{}
	}
	var parallel *bool
	if raw, ok := req.Extra["parallel_tool_calls"]; ok {
		p, err := parseParallelToolCalls(raw)
		if err != nil {
			return nil, err
		}
		parallel = p
		consumed["parallel_tool_calls"] = struct{}{}
	}
	if disableTools {
		out.Tools = nil
		out.ToolChoice = nil
	} else if len(out.Tools) > 0 {
		// parallel_tool_calls=false implicitly forces a tool_choice so
		// Anthropic accepts disable_parallel_tool_use; auto is the
		// neutral default.
		if out.ToolChoice == nil && parallel != nil && !*parallel {
			out.ToolChoice = &anthropicToolChoice{Type: "auto"}
		}
		out.ToolChoice = applyParallelToolCalls(out.ToolChoice, parallel)
	}

	body, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	if len(req.Extra) == 0 {
		return body, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	for key, value := range req.Extra {
		if _, taken := consumed[key]; taken {
			continue
		}
		if _, ok := raw[key]; ok {
			continue
		}
		raw[key] = value
	}
	return json.Marshal(raw)
}

// buildMessageContent converts one llmtypes.Message into the Anthropic
// content shape. Plain text messages stay as a string (Anthropic accepts
// both string and array). Assistant messages with prior tool_calls become
// an array of [text?, tool_use*]. Tool-role messages become a single
// tool_result block; the caller switches role to "user" since Anthropic
// has no dedicated tool role.
func buildMessageContent(msg llmtypes.Message) (any, error) {
	if len(msg.ContentRaw) > 0 {
		return nil, errors.New("anthropic provider does not support OpenAI structured message content")
	}
	if msg.Role == "tool" {
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
		name := strings.TrimSpace(tc.Function.Name)
		if name == "" {
			return nil, errors.New("tool_call.function.name is required")
		}
		input, err := parseToolCallArguments(tc.Function.Arguments)
		if err != nil {
			return nil, fmt.Errorf("tool_call %q arguments: %w", name, err)
		}
		blocks = append(blocks, anthropicContentBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  name,
			Input: input,
		})
	}
	return blocks, nil
}
