package anthropic

import (
	"encoding/json"
	"errors"
	"strings"

	"llmgate/internal/domain/llmtypes"
)

func toOpenAIResponse(in *anthropicResponse) (*llmtypes.Response, error) {
	if in == nil {
		return nil, errors.New("response is nil")
	}
	var text strings.Builder
	var reasoning strings.Builder
	for _, block := range in.Content {
		switch block.Type {
		case "thinking":
			if block.Thinking != "" {
				reasoning.WriteString(block.Thinking)
			} else {
				reasoning.WriteString(block.Text)
			}
		case "tool_use":
			// handled separately via extractToolCalls; do not feed
			// the tool_use block's empty Text into the text builder.
		default:
			text.WriteString(block.Text)
		}
	}

	finishReason := ""
	if in.StopReason != nil {
		finishReason = mapStopReason(*in.StopReason)
	}
	usage := anthropicUsageToOpenAI(in.Usage)

	msg := llmtypes.Message{
		Role:             "assistant",
		Content:          text.String(),
		ReasoningContent: reasoning.String(),
	}
	if toolCalls := extractToolCalls(in.Content); len(toolCalls) > 0 {
		raw, err := json.Marshal(toolCalls)
		if err != nil {
			return nil, err
		}
		msg.Extra = map[string]json.RawMessage{"tool_calls": raw}
	}

	return &llmtypes.Response{
		ID:     in.ID,
		Object: "chat.completion",
		Model:  in.Model,
		Choices: []llmtypes.Choice{{
			Index:        0,
			Message:      msg,
			FinishReason: finishReason,
		}},
		Usage: usage,
	}, nil
}

// extractToolCalls maps Anthropic tool_use content blocks to the OpenAI
// tool_calls wire shape. Each call's Arguments is re-serialized to a
// canonical JSON string (OpenAI requires arguments as a string, not an
// object); malformed JSON falls back to the trimmed raw bytes, and
// empty / missing input collapses to "{}" so downstream parsers always
// see a valid object literal. No path can fail — this is a structural
// reshape of already-decoded data.
func extractToolCalls(blocks []anthropicContent) []map[string]any {
	out := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		if b.Type != "tool_use" || b.Name == "" {
			continue
		}
		arguments := "{}"
		if len(b.Input) > 0 {
			var parsed any
			if err := json.Unmarshal(b.Input, &parsed); err == nil {
				if canonical, err := json.Marshal(parsed); err == nil {
					arguments = string(canonical)
				}
			} else if trimmed := strings.TrimSpace(string(b.Input)); trimmed != "" {
				arguments = trimmed
			}
		}
		out = append(out, map[string]any{
			"id":   b.ID,
			"type": "function",
			"function": map[string]any{
				"name":      b.Name,
				"arguments": arguments,
			},
		})
	}
	return out
}

// mapStopReason translates Anthropic's stop_reason vocabulary into the
// OpenAI finish_reason enum so SDK clients that strictly enum-validate
// the field don't break when Anthropic ships a new value. Unknown
// values fall back to "stop" — losing the original label is preferable
// to leaking a non-enum string. Update this whitelist when Anthropic
// adds values; an empty input is preserved as empty so callers can
// distinguish "not set yet" from "stopped".
func mapStopReason(s string) string {
	switch s {
	case "":
		return ""
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		// Anthropic "refusal" is the model declining per its own policy;
		// OpenAI "content_filter" is the closest semantic in the OpenAI
		// enum (a content-policy-driven stop).
		return "content_filter"
	case "pause_turn":
		// pause_turn signals that more turns are coming (e.g. extended
		// thinking continues across a follow-up request). No exact OpenAI
		// equivalent — clients see "stop" and the next request continues.
		return "stop"
	default:
		return "stop"
	}
}

func anthropicUsageToOpenAI(in anthropicUsage) *llmtypes.Usage {
	usage := &llmtypes.Usage{
		PromptTokens:     in.InputTokens,
		CompletionTokens: in.OutputTokens,
		TotalTokens:      in.InputTokens + in.OutputTokens,
	}
	addCacheUsageExtra(usage, in.CacheCreationInputTokens, in.CacheReadInputTokens)
	return usage
}

func addCacheUsageExtra(usage *llmtypes.Usage, cacheCreationTokens, cacheReadTokens int) {
	if cacheCreationTokens <= 0 && cacheReadTokens <= 0 {
		return
	}
	usage.Extra = make(map[string]json.RawMessage)
	if cacheCreationTokens > 0 {
		usage.Extra["cache_creation_input_tokens"] = json.RawMessage(jsonInt(cacheCreationTokens))
	}
	if cacheReadTokens > 0 {
		usage.Extra["cache_read_input_tokens"] = json.RawMessage(jsonInt(cacheReadTokens))
	}
}

func jsonInt(n int) []byte {
	b, _ := json.Marshal(n)
	return b
}
