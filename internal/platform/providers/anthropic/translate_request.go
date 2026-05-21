package anthropic

import (
	"encoding/json"
	"errors"
	"strings"

	"llmgate/internal/domain/llmtypes"
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
		converted, err := anthropicMessageFromOpenAI(msg)
		if err != nil {
			return nil, err
		}
		messages = append(messages, converted)
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
