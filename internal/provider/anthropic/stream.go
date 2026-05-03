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

// Recv pulls the next OpenAI-shaped chunk out of the Anthropic SSE
// stream. It is structured as: (1) flush any deferred finish event
// from the prior call, (2) scan the next data line and dispatch by
// event.Type to a per-event handler, (3) run finalize when the
// scanner runs dry. Each per-event handler is small and single-purpose
// so the state-machine surface stays readable.
func (s *stream) Recv() (*provider.Event, error) {
	if s.closed.Load() {
		return nil, io.EOF
	}
	if s.pendingFinish != nil && !s.pendingEmitted {
		return s.emitFinish(), nil
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

		event, err := s.decodePayload(payload)
		if err != nil {
			return nil, err
		}

		emitted, evt, err := s.dispatch(event, payload)
		if err != nil {
			return nil, err
		}
		if emitted {
			return evt, nil
		}
	}

	return s.finalize()
}

// emitFinish flushes the buffered finish event exactly once, advancing
// internal state so the next Recv returns io.EOF.
func (s *stream) emitFinish() *provider.Event {
	s.pendingEmitted = true
	s.recordEmit()
	return s.buildFinishEvent(s.pendingFinish)
}

// decodePayload parses a single SSE data payload into the wire-shape
// anthropicStreamEvent. Failures wrap into a typed *provider.Error so
// the loop can return immediately.
func (s *stream) decodePayload(payload []byte) (*anthropicStreamEvent, error) {
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
	return &event, nil
}

// dispatch routes one decoded event by type to its handler. The
// (emitted, evt, err) tuple distinguishes three outcomes:
//   - emitted=true, evt!=nil, err=nil  → caller returns evt
//   - emitted=false, err!=nil          → caller returns err (terminal)
//   - emitted=false, err=nil           → caller continues scanning
//
// payload is passed through to the error-event handlers because they
// re-parse the envelope shape that anthropicStreamEvent doesn't model.
func (s *stream) dispatch(event *anthropicStreamEvent, payload []byte) (emitted bool, evt *provider.Event, err error) {
	switch event.Type {
	case "message_start":
		return true, s.handleMessageStart(event), nil
	case "content_block_delta":
		return s.handleContentBlockDelta(event)
	case "message_delta":
		s.handleMessageDelta(event)
		return false, nil, nil
	case "message_stop":
		return true, s.handleMessageStop(), nil
	case "ping", "content_block_start", "content_block_stop":
		return false, nil, nil
	case "error":
		return false, nil, errorFromStreamEvent(payload, s.providerName)
	default:
		if perr := parseMaybeStreamError(payload, s.providerName); perr != nil {
			return false, nil, perr
		}
		return false, nil, nil
	}
}

// handleMessageStart captures message metadata + input usage and emits
// the assistant-role chunk that opens an OpenAI-shaped stream.
func (s *stream) handleMessageStart(event *anthropicStreamEvent) *provider.Event {
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
	}
}

// handleContentBlockDelta translates one Anthropic delta block to an
// OpenAI delta chunk. text_delta becomes Content; thinking_delta
// becomes ReasoningContent (with text fallback when Thinking is empty
// — older API shape). Unknown delta types are silently skipped.
func (s *stream) handleContentBlockDelta(event *anthropicStreamEvent) (bool, *provider.Event, error) {
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
		return false, nil, nil
	}
	s.recordEmit()
	return true, &provider.Event{
		ID:     s.msgID,
		Object: "chat.completion.chunk",
		Model:  s.msgModel,
		Choices: []provider.ChoiceDelta{{
			Index: 0,
			Delta: delta,
		}},
	}, nil
}

// handleMessageDelta buffers the finish reason and output usage so the
// terminal message_stop (or post-loop fallback) can build the final
// chunk. Does not emit on its own.
func (s *stream) handleMessageDelta(event *anthropicStreamEvent) {
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
}

// handleMessageStop emits the terminal finish chunk. If message_delta
// never arrived (server cut early after a content block), synthesize a
// generic "stop" reason so callers still see a clean finish.
func (s *stream) handleMessageStop() *provider.Event {
	if s.pendingFinish == nil {
		s.pendingFinish = &anthropicEnd{finishReason: "stop"}
	}
	s.pendingEmitted = true
	s.recordEmit()
	return s.buildFinishEvent(s.pendingFinish)
}

// finalize handles the post-loop state. Scanner errors win. If we
// have a buffered finish but never saw message_stop, surface it as the
// final chunk. Otherwise treat the abrupt end as an upstream fault.
func (s *stream) finalize() (*provider.Event, error) {
	if err := s.scanner.Err(); err != nil {
		return nil, &provider.Error{
			Kind:     provider.KindUpstream,
			Provider: s.providerName,
			Message:  err.Error(),
			Cause:    err,
		}
	}
	if s.pendingFinish != nil && !s.pendingEmitted {
		return s.emitFinish(), nil
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
