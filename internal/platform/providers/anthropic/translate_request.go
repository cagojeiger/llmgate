package anthropic

import (
	"encoding/json"
	"errors"
	"strings"

	"llmgate/internal/domain/llmtypes"
)

// openAIOnlyRequestKeys are OpenAI-wire request fields with no Anthropic
// equivalent. A strict Anthropic-protocol upstream rejects unknown top-level
// params with a 400, which is not fallback-eligible — one leaked key would
// kill the whole alias chain. The current opencode upstream happens to ignore
// them (probed 2026-07-11), but the gate drops them so chain survival never
// depends on upstream leniency.
var openAIOnlyRequestKeys = map[string]struct{}{
	"frequency_penalty":  {},
	"presence_penalty":   {},
	"logit_bias":         {},
	"logprobs":           {},
	"top_logprobs":       {},
	"n":                  {},
	"response_format":    {},
	"stream_options":     {},
	"store":              {},
	"metadata":           {}, // OpenAI map shape; Anthropic metadata is {user_id}
	"modalities":         {},
	"audio":              {},
	"prediction":         {},
	"web_search_options": {},
	"service_tier":       {}, // value sets differ between the two wires
	"reasoning_effort":   {},
	"verbosity":          {},
	"functions":          {}, // legacy pre-tools API
	"function_call":      {},
	"safety_identifier":  {},
	"prompt_cache_key":   {},
}

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

	// Tool support: read OpenAI-shaped fields from Extra, translate, and
	// record them as consumed so the trailing Extra-merge does not leak
	// the OpenAI shape back onto the wire.
	consumed := map[string]struct{}{}

	maxTokens := req.MaxTokens
	if raw, ok := req.Extra["max_completion_tokens"]; ok {
		// Newer OpenAI SDKs send max_completion_tokens instead of max_tokens.
		var v int
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, errors.New("max_completion_tokens must be an integer")
		}
		if maxTokens == 0 && v > 0 {
			maxTokens = v
		}
		consumed["max_completion_tokens"] = struct{}{}
	}
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
		if _, drop := openAIOnlyRequestKeys[key]; drop {
			continue
		}
		if _, ok := raw[key]; ok {
			continue
		}
		raw[key] = value
	}
	return json.Marshal(raw)
}
