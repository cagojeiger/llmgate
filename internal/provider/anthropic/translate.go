package anthropic

import (
	"encoding/json"
	"errors"
	"strings"

	"llmgate/internal/provider"
)

type anthropicRequest struct {
	Model         string             `json:"model"`
	Messages      []anthropicMessage `json:"messages"`
	System        string             `json:"system,omitempty"`
	MaxTokens     int                `json:"max_tokens"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicResponse struct {
	ID           string             `json:"id"`
	Type         string             `json:"type"`
	Role         string             `json:"role"`
	Model        string             `json:"model"`
	Content      []anthropicContent `json:"content"`
	StopReason   *string            `json:"stop_reason"`
	StopSequence *string            `json:"stop_sequence"`
	Usage        anthropicUsage     `json:"usage"`
}

type anthropicContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

func toAnthropicRequest(req *provider.Request, defaultMaxTokens int, stream bool) ([]byte, error) {
	var system []string
	messages := make([]anthropicMessage, 0, len(req.Messages))
	for _, msg := range req.Messages {
		if msg.Role == "system" {
			system = append(system, msg.Content)
			continue
		}
		content, err := json.Marshal(msg.Content)
		if err != nil {
			return nil, err
		}
		messages = append(messages, anthropicMessage{
			Role:    msg.Role,
			Content: json.RawMessage(content),
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
		if _, ok := raw[key]; ok {
			continue
		}
		raw[key] = value
	}
	return json.Marshal(raw)
}

func toOpenAIResponse(in *anthropicResponse) (*provider.Response, error) {
	if in == nil {
		return nil, errors.New("response is nil")
	}
	var text strings.Builder
	for _, block := range in.Content {
		text.WriteString(block.Text)
	}

	finishReason := ""
	if in.StopReason != nil {
		finishReason = mapStopReason(*in.StopReason)
	}
	usage := anthropicUsageToOpenAI(in.Usage)

	return &provider.Response{
		ID:     in.ID,
		Object: "chat.completion",
		Model:  in.Model,
		Choices: []provider.Choice{{
			Index: 0,
			Message: provider.Message{
				Role:    "assistant",
				Content: text.String(),
			},
			FinishReason: finishReason,
		}},
		Usage: usage,
	}, nil
}

func mapStopReason(s string) string {
	switch s {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return s
	}
}

func anthropicUsageToOpenAI(in anthropicUsage) *provider.Usage {
	usage := &provider.Usage{
		PromptTokens:     in.InputTokens,
		CompletionTokens: in.OutputTokens,
		TotalTokens:      in.InputTokens + in.OutputTokens,
	}
	addCacheUsageExtra(usage, in.CacheCreationInputTokens, in.CacheReadInputTokens)
	return usage
}

func addCacheUsageExtra(usage *provider.Usage, cacheCreationTokens, cacheReadTokens int) {
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
