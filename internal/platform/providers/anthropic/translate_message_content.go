package anthropic

import (
	"bytes"
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
	if msg.Role == "tool" {
		return buildToolResultContent(msg)
	}
	return buildAssistantContent(msg)
}

// isStructuredContentArray reports whether raw is an OpenAI content-parts array.
func isStructuredContentArray(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '['
}

// openAIContentPart is one entry in OpenAI's structured message content array,
// e.g. {"type":"text","text":...} or {"type":"image_url","image_url":{"url":...}}.
type openAIContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

// buildStructuredContent translates a standalone OpenAI content-parts array
// into Anthropic content blocks. An empty array is rejected — Anthropic 400s
// on empty message content, and a clear local error beats that.
func buildStructuredContent(raw json.RawMessage) (any, error) {
	blocks, err := structuredContentBlocks(raw)
	if err != nil {
		return nil, err
	}
	if len(blocks) == 0 {
		return nil, errors.New("message content array is empty")
	}
	return blocks, nil
}

// structuredContentBlocks translates OpenAI's content-parts array into Anthropic
// content blocks: text parts pass through; image_url parts become image blocks
// (base64 source from a data: URI, url source otherwise).
func structuredContentBlocks(raw json.RawMessage) ([]anthropicContentBlock, error) {
	var parts []openAIContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("decode structured message content: %w", err)
	}
	blocks := make([]anthropicContentBlock, 0, len(parts))
	for i, part := range parts {
		switch part.Type {
		case "text":
			blocks = append(blocks, anthropicContentBlock{Type: "text", Text: part.Text})
		case "image_url":
			source, err := imageSourceFromURL(part.ImageURL.URL)
			if err != nil {
				return nil, fmt.Errorf("content part %d: %w", i, err)
			}
			blocks = append(blocks, anthropicContentBlock{Type: "image", Source: source})
		default:
			return nil, fmt.Errorf("content part %d: unsupported type %q", i, part.Type)
		}
	}
	return blocks, nil
}

// imageSourceFromURL maps an OpenAI image_url.url to an Anthropic image source.
// A data: URI (data:<media_type>;base64,<data>) becomes a base64 source; any
// other URL is passed through as a url source for Anthropic to fetch.
func imageSourceFromURL(url string) (*anthropicImageSource, error) {
	url = strings.TrimSpace(url)
	if url == "" {
		return nil, errors.New("image_url.url is empty")
	}
	if !strings.HasPrefix(url, "data:") {
		return &anthropicImageSource{Type: "url", URL: url}, nil
	}
	meta, data, ok := strings.Cut(strings.TrimPrefix(url, "data:"), ",")
	if !ok {
		return nil, errors.New("malformed data URI: missing comma")
	}
	// meta is <media-type>[;param=value...][;base64]. Require base64 and keep
	// only the bare media type — Anthropic rejects a media_type that carries
	// parameters (e.g. "image/png;charset=utf-8").
	if !strings.Contains(meta, ";base64") {
		return nil, errors.New("data URI must be base64-encoded")
	}
	mediaType, _, _ := strings.Cut(meta, ";")
	if mediaType == "" {
		return nil, errors.New("data URI is missing a media type")
	}
	// Some encoders wrap base64 at column 76; Anthropic wants it unbroken.
	data = strings.NewReplacer("\n", "", "\r", "").Replace(data)
	return &anthropicImageSource{Type: "base64", MediaType: mediaType, Data: data}, nil
}

func buildToolResultContent(msg llmtypes.Message) (any, error) {
	toolCallID, err := extractStringField(msg.Extra, "tool_call_id")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(toolCallID) == "" {
		return nil, errors.New("tool message is missing tool_call_id")
	}
	content := msg.Content
	if isStructuredContentArray(msg.ContentRaw) {
		text, err := flattenTextParts(msg.ContentRaw)
		if err != nil {
			return nil, err
		}
		content = text
	}
	return []anthropicContentBlock{{
		Type:      "tool_result",
		ToolUseID: toolCallID,
		Content:   content,
	}}, nil
}

// flattenTextParts concatenates an OpenAI content-parts array into a string.
// Tool results are text in practice; a non-text part is rejected rather than
// silently dropped.
func flattenTextParts(raw json.RawMessage) (string, error) {
	var parts []openAIContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", fmt.Errorf("decode tool result content: %w", err)
	}
	var b strings.Builder
	for i, part := range parts {
		if part.Type != "text" {
			return "", fmt.Errorf("tool result part %d: unsupported type %q", i, part.Type)
		}
		b.WriteString(part.Text)
	}
	return b.String(), nil
}

// buildAssistantContent handles user and assistant turns. With no tool_calls it
// returns structured content blocks (image/text) or the plain string; with
// tool_calls it emits those leading blocks followed by tool_use blocks.
func buildAssistantContent(msg llmtypes.Message) (any, error) {
	toolCalls, err := toolCallsFromExtra(msg.Extra)
	if err != nil {
		return nil, err
	}
	if len(toolCalls) == 0 {
		if isStructuredContentArray(msg.ContentRaw) {
			return buildStructuredContent(msg.ContentRaw)
		}
		return msg.Content, nil
	}

	blocks := make([]anthropicContentBlock, 0, len(toolCalls)+1)
	if isStructuredContentArray(msg.ContentRaw) {
		leading, err := structuredContentBlocks(msg.ContentRaw)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, leading...)
	} else if strings.TrimSpace(msg.Content) != "" {
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

// toolCallsFromExtra returns the OpenAI tool_calls from Extra, or nil when absent
// or empty (including tool_calls: []).
func toolCallsFromExtra(extra map[string]json.RawMessage) ([]openAIToolCall, error) {
	raw, ok := extra["tool_calls"]
	if !ok || len(raw) == 0 {
		return nil, nil
	}
	var toolCalls []openAIToolCall
	if err := json.Unmarshal(raw, &toolCalls); err != nil {
		return nil, fmt.Errorf("decode tool_calls: %w", err)
	}
	return toolCalls, nil
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
