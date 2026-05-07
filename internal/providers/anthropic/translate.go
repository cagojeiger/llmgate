package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"llmgate/internal/llmtypes"
)

type anthropicRequest struct {
	Model         string               `json:"model"`
	Messages      []anthropicMessage   `json:"messages"`
	System        string               `json:"system,omitempty"`
	MaxTokens     int                  `json:"max_tokens"`
	Temperature   *float64             `json:"temperature,omitempty"`
	TopP          *float64             `json:"top_p,omitempty"`
	StopSequences []string             `json:"stop_sequences,omitempty"`
	Stream        bool                 `json:"stream,omitempty"`
	Tools         []anthropicTool      `json:"tools,omitempty"`
	ToolChoice    *anthropicToolChoice `json:"tool_choice,omitempty"`
}

// anthropicMessage uses any for Content so callers can pass either a plain
// string (Anthropic accepts both shapes) or a slice of content blocks for
// tool_use / tool_result turns. json.Marshal handles either correctly.
type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// anthropicContentBlock is the structured-content variant carried inside
// anthropicMessage.Content. Fields are union-shaped (only the ones
// relevant to the block Type are populated); omitempty keeps the wire
// minimal for the common text-only case.
type anthropicContentBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Input     any    `json:"input,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   any    `json:"content,omitempty"`
}

// anthropicTool is the wire shape of one entry in Anthropic /v1/messages
// `tools` array. Note input_schema (Anthropic) ↔ parameters (OpenAI).
type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

// anthropicToolChoice is the wire shape of /v1/messages tool_choice.
// Type is one of: "auto" | "any" | "tool" — note OpenAI "required" maps
// to "any". DisableParallelToolUse expresses OpenAI's parallel_tool_calls
// = false (Anthropic models invoke tools sequentially when set).
type anthropicToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse *bool  `json:"disable_parallel_tool_use,omitempty"`
}

type anthropicResponse struct {
	ID         string             `json:"id"`
	Type       string             `json:"type"`
	Role       string             `json:"role"`
	Model      string             `json:"model"`
	Content    []anthropicContent `json:"content"`
	StopReason *string            `json:"stop_reason"`
	Usage      anthropicUsage     `json:"usage"`
}

type anthropicContent struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	Thinking string          `json:"thinking,omitempty"`
	ID       string          `json:"id,omitempty"`
	Name     string          `json:"name,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

func toAnthropicRequest(req *llmtypes.Request, defaultMaxTokens int, stream bool) ([]byte, error) {
	var system []string
	messages := make([]anthropicMessage, 0, len(req.Messages))
	for _, msg := range req.Messages {
		if msg.Role == "system" {
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
	var out []map[string]any
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
