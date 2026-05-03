package anthropic

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"llmgate/internal/provider"
	"llmgate/internal/provider/httpx"
)

func (c *Client) CompleteStream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	if err := req.Validate(); err != nil {
		return nil, provider.StampProvider(err, c.cfg.Name)
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
	closed         atomic.Bool
	closeOnce      sync.Once
	closeErr       error
	msgID          string
	msgModel       string
	inputTokens    int
	pendingFinish  *anthropicEnd
	pendingEmitted bool
	providerName   string

	// accumulated state for Summary()
	chunkCount  int
	firstByteAt time.Time
}

func (s *stream) recordEmit() {
	if s.firstByteAt.IsZero() {
		s.firstByteAt = time.Now()
	}
	s.chunkCount++
}

type anthropicEnd struct {
	finishReason        string
	outputTokens        int
	cacheCreationTokens int
	cacheReadTokens     int
}

func (s *stream) Recv() (*provider.Event, error) {
	if s.closed.Load() {
		return nil, io.EOF
	}
	if s.pendingFinish != nil && !s.pendingEmitted {
		s.pendingEmitted = true
		s.recordEmit()
		return s.buildFinishEvent(s.pendingFinish), nil
	}
	if s.pendingEmitted {
		s.closed.Store(true)
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
				Raw:      httpx.FirstBytes(payload),
			}
		}

		switch event.Type {
		case "message_start":
			if event.Message != nil {
				s.msgID = event.Message.ID
				s.msgModel = event.Message.Model
				s.inputTokens = event.Message.Usage.InputTokens
			}
			s.recordEmit()
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
			var delta provider.Delta
			switch event.Delta.Type {
			case "text_delta":
				delta.Content = event.Delta.Text
			case "thinking_delta":
				delta.ReasoningContent = event.Delta.Thinking
				if delta.ReasoningContent == "" {
					delta.ReasoningContent = event.Delta.Text
				}
			default:
				continue
			}
			s.recordEmit()
			return &provider.Event{
				ID:     s.msgID,
				Object: "chat.completion.chunk",
				Model:  s.msgModel,
				Choices: []provider.ChoiceDelta{{
					Index: 0,
					Delta: delta,
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
			s.recordEmit()
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
		s.recordEmit()
		return s.buildFinishEvent(s.pendingFinish), nil
	}
	return nil, &provider.Error{
		Kind:     provider.KindUpstream,
		Provider: s.providerName,
		Message:  "stream ended without message_stop",
	}
}

func (s *stream) Summary() *provider.Summary {
	summary := &provider.Summary{
		Model:       s.msgModel,
		ChunkCount:  s.chunkCount,
		FirstByteAt: s.firstByteAt,
	}
	if s.pendingFinish != nil {
		summary.FinishReason = s.pendingFinish.finishReason
		usage := &provider.Usage{
			PromptTokens:     s.inputTokens,
			CompletionTokens: s.pendingFinish.outputTokens,
			TotalTokens:      s.inputTokens + s.pendingFinish.outputTokens,
		}
		addCacheUsageExtra(usage, s.pendingFinish.cacheCreationTokens, s.pendingFinish.cacheReadTokens)
		summary.Usage = usage
	} else if s.inputTokens > 0 {
		// Partial: only message_start arrived. Surface what we got so audit
		// can record prompt token consumption even when generation aborted.
		summary.Usage = &provider.Usage{
			PromptTokens: s.inputTokens,
			TotalTokens:  s.inputTokens,
		}
	}
	return summary
}

func (s *stream) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		if s.body != nil {
			s.closeErr = s.body.Close()
		}
	})
	return s.closeErr
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
		Text         string  `json:"text,omitempty"`
		Thinking     string  `json:"thinking,omitempty"`
		StopReason   *string `json:"stop_reason"`
		StopSequence *string `json:"stop_sequence"`
	} `json:"delta"`
	Usage anthropicUsage `json:"usage"`
}

func dataPayload(line string) []byte {
	if !strings.HasPrefix(line, "data:") {
		return nil
	}
	payload := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
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
