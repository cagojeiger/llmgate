package anthropic

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"

	"llmgate/internal/provider"
)

func (c *Client) CompleteStream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	if err := req.Validate(); err != nil {
		return nil, withProvider(err, c.cfg.Name)
	}

	body, err := toAnthropicRequest(req, c.cfg.DefaultMaxTokens, true)
	if err != nil {
		return nil, c.badRequest("translate request", err, nil)
	}

	httpReq, err := c.newRequest(ctx, "text/event-stream", body)
	if err != nil {
		return nil, c.badRequest("build request", err, nil)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, c.lowLevelError("send request", err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, c.lowLevelError("read error response", err)
		}
		return nil, c.classify(resp.StatusCode, raw, resp.Header.Get("Retry-After"))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	return &stream{
		body:         resp.Body,
		scanner:      scanner,
		providerName: c.cfg.Name,
	}, nil
}

type stream struct {
	body           io.ReadCloser
	scanner        *bufio.Scanner
	closed         bool
	msgID          string
	msgModel       string
	inputTokens    int
	pendingFinish  *anthropicEnd
	pendingEmitted bool
	providerName   string
}

type anthropicEnd struct {
	finishReason        string
	outputTokens        int
	cacheCreationTokens int
	cacheReadTokens     int
}

func (s *stream) Recv() (*provider.Event, error) {
	if s.closed {
		return nil, io.EOF
	}
	if s.pendingFinish != nil && !s.pendingEmitted {
		s.pendingEmitted = true
		return s.buildFinishEvent(s.pendingFinish), nil
	}
	if s.pendingEmitted {
		s.closed = true
		return nil, io.EOF
	}

	for s.scanner.Scan() {
		payload := dataPayload(s.scanner.Text())
		if len(payload) == 0 {
			continue
		}
		if string(payload) == "[DONE]" {
			break
		}

		var event anthropicStreamEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			return nil, &provider.Error{
				Kind:     provider.KindUpstream,
				Provider: s.providerName,
				Message:  "decode stream event: " + err.Error(),
				Cause:    err,
				Raw:      firstBytes(payload),
			}
		}

		switch event.Type {
		case "message_start":
			if event.Message != nil {
				s.msgID = event.Message.ID
				s.msgModel = event.Message.Model
				s.inputTokens = event.Message.Usage.InputTokens
			}
			return &provider.Event{
				ID:     s.msgID,
				Object: "chat.completion.chunk",
				Model:  s.msgModel,
				Choices: []provider.ChoiceDelta{{
					Index: 0,
					Delta: provider.Delta{Role: "assistant"},
				}},
			}, nil
		case "content_block_delta":
			if event.Delta.Type != "text_delta" {
				continue
			}
			return &provider.Event{
				ID:     s.msgID,
				Object: "chat.completion.chunk",
				Model:  s.msgModel,
				Choices: []provider.ChoiceDelta{{
					Index: 0,
					Delta: provider.Delta{Content: event.Delta.Text},
				}},
			}, nil
		case "message_delta":
			finishReason := ""
			if event.Delta.StopReason != nil {
				finishReason = mapStopReason(*event.Delta.StopReason)
			}
			s.pendingFinish = &anthropicEnd{
				finishReason:        finishReason,
				outputTokens:        event.Usage.OutputTokens,
				cacheCreationTokens: event.Usage.CacheCreationInputTokens,
				cacheReadTokens:     event.Usage.CacheReadInputTokens,
			}
			continue
		case "message_stop":
			if s.pendingFinish == nil {
				s.pendingFinish = &anthropicEnd{finishReason: "stop"}
			}
			s.pendingEmitted = true
			return s.buildFinishEvent(s.pendingFinish), nil
		case "ping", "content_block_start", "content_block_stop":
			continue
		case "error":
			return nil, errorFromStreamEvent(payload, s.providerName)
		default:
			if perr := parseMaybeStreamError(payload, s.providerName); perr != nil {
				return nil, perr
			}
			continue
		}
	}

	if err := s.scanner.Err(); err != nil {
		return nil, &provider.Error{
			Kind:     provider.KindUpstream,
			Provider: s.providerName,
			Message:  err.Error(),
			Cause:    err,
		}
	}
	if s.pendingFinish != nil && !s.pendingEmitted {
		s.pendingEmitted = true
		return s.buildFinishEvent(s.pendingFinish), nil
	}
	return nil, &provider.Error{
		Kind:     provider.KindUpstream,
		Provider: s.providerName,
		Message:  "stream ended without message_stop",
	}
}

func (s *stream) Close() error {
	s.closed = true
	if s.body == nil {
		return nil
	}
	body := s.body
	s.body = nil
	return body.Close()
}

func (s *stream) buildFinishEvent(end *anthropicEnd) *provider.Event {
	if end == nil {
		end = &anthropicEnd{finishReason: "stop"}
	}
	usage := &provider.Usage{
		PromptTokens:     s.inputTokens,
		CompletionTokens: end.outputTokens,
		TotalTokens:      s.inputTokens + end.outputTokens,
	}
	addCacheUsageExtra(usage, end.cacheCreationTokens, end.cacheReadTokens)
	return &provider.Event{
		ID:     s.msgID,
		Object: "chat.completion.chunk",
		Model:  s.msgModel,
		Choices: []provider.ChoiceDelta{{
			Index:        0,
			Delta:        provider.Delta{},
			FinishReason: end.finishReason,
		}},
		Usage: usage,
	}
}

type anthropicStreamEvent struct {
	Type    string             `json:"type"`
	Message *anthropicResponse `json:"message,omitempty"`
	Delta   struct {
		Type         string  `json:"type"`
		Text         string  `json:"text"`
		StopReason   *string `json:"stop_reason"`
		StopSequence *string `json:"stop_sequence"`
	} `json:"delta"`
	Usage anthropicUsage `json:"usage"`
}

func dataPayload(line string) []byte {
	if !strings.HasPrefix(line, "data:") {
		return nil
	}
	payload := strings.TrimPrefix(line, "data:")
	if strings.HasPrefix(payload, " ") {
		payload = payload[1:]
	}
	if payload == "" {
		return nil
	}
	return []byte(payload)
}

func parseMaybeStreamError(payload []byte, providerName string) *provider.Error {
	message, _ := envelopeMessage(payload)
	if message == "" {
		return nil
	}
	return errorFromStreamEvent(payload, providerName)
}
