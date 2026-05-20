package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync/atomic"

	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/streaming"
	"llmgate/internal/platform/upstream"
)

func (c *Client) CompleteStream(ctx context.Context, req *llmtypes.Request) (llmtypes.Stream, error) {
	if err := req.Validate(); err != nil {
		return nil, llmtypes.StampProvider(err, c.cfg.Name)
	}

	body, err := toAnthropicRequest(req, c.cfg.DefaultMaxTokens, true)
	if err != nil {
		return nil, c.badRequest("translate request", err, nil)
	}

	httpReq, err := c.newRequest(ctx, "text/event-stream", body)
	if err != nil {
		return nil, c.badRequest("build request", err, nil)
	}

	resp, statusErr, err := upstream.OpenSSE(c.http, httpReq, c.cfg.Name)
	if err != nil {
		return nil, err
	}
	if statusErr != nil {
		return nil, c.classify(statusErr.Status, statusErr.Body, statusErr.RetryAfter)
	}

	return streaming.ValidateStreamStart(ctx, &stream{
		StreamBase: streaming.StreamBase{
			Body:         resp.Body,
			ProviderName: c.cfg.Name,
		},
		reader:    upstream.NewSSEReader(resp.Body),
		toolCalls: make(map[int]*streamToolCallState),
	})
}

type stream struct {
	streaming.StreamBase

	reader *upstream.SSEReader
	closed atomic.Bool

	// per-stream protocol state (anthropic-specific)
	msgID          string
	msgModel       string
	inputTokens    int
	pendingFinish  *anthropicEnd
	pendingEmitted bool

	// tool_use accumulator. Anthropic announces each tool call as a
	// separate content_block_start (type=tool_use) keyed by an index that
	// is unique within the message; we map that index to per-call state
	// so subsequent input_json_delta events can find the right slot. The
	// OpenAI tool_calls delta requires its own zero-based index, which we
	// allocate via nextToolCallIndex.
	toolCalls         map[int]*streamToolCallState
	nextToolCallIndex int
}

// Close marks the stream closed (so a blocked Recv returns EOF) before
// delegating to StreamBase for the actual body close.
func (s *stream) Close() error {
	s.closed.Store(true)
	return s.StreamBase.Close()
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
func (s *stream) Recv() (*llmtypes.Event, error) {
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

	for {
		payload, err := s.reader.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(payload) == 0 {
			continue
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
func (s *stream) emitFinish() *llmtypes.Event {
	s.pendingEmitted = true
	s.RecordEmit()
	return s.buildFinishEvent(s.pendingFinish)
}

// decodePayload parses a single SSE data payload into the wire-shape
// anthropicStreamEvent. Failures wrap into a typed *llmtypes.Error so
// the loop can return immediately.
func (s *stream) decodePayload(payload []byte) (*anthropicStreamEvent, error) {
	var event anthropicStreamEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, &llmtypes.Error{
			Kind:     llmtypes.KindUpstream,
			Provider: s.ProviderName,
			Message:  "upstream returned invalid response",
			Cause:    err,
			Raw:      upstream.FirstBytes(payload),
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
func (s *stream) dispatch(event *anthropicStreamEvent, payload []byte) (emitted bool, evt *llmtypes.Event, err error) {
	switch event.Type {
	case "message_start":
		return true, s.handleMessageStart(event), nil
	case "content_block_start":
		return s.handleContentBlockStart(event)
	case "content_block_delta":
		return s.handleContentBlockDelta(event)
	case "content_block_stop":
		return s.handleContentBlockStop(event)
	case "message_delta":
		s.handleMessageDelta(event)
		return false, nil, nil
	case "message_stop":
		return true, s.handleMessageStop(), nil
	case "ping":
		return false, nil, nil
	case "error":
		return false, nil, errorFromStreamEvent(payload, s.ProviderName)
	default:
		if perr := parseMaybeStreamError(payload, s.ProviderName); perr != nil {
			return false, nil, perr
		}
		return false, nil, nil
	}
}

// handleMessageStart captures message metadata + input usage and emits
// the assistant-role chunk that opens an OpenAI-shaped stream.
func (s *stream) handleMessageStart(event *anthropicStreamEvent) *llmtypes.Event {
	if event.Message != nil {
		s.msgID = event.Message.ID
		s.msgModel = event.Message.Model
		s.inputTokens = event.Message.Usage.InputTokens
	}
	s.RecordEmit()
	return &llmtypes.Event{
		ID:     s.msgID,
		Object: "chat.completion.chunk",
		Model:  s.msgModel,
		Choices: []llmtypes.ChoiceDelta{{
			Index: 0,
			Delta: llmtypes.Delta{Role: "assistant"},
		}},
	}
}

// handleContentBlockDelta translates one Anthropic delta block to an
// OpenAI delta chunk. text_delta becomes Content; thinking_delta
// becomes ReasoningContent (with text fallback when Thinking is empty
// — older API shape); input_json_delta extends the matching tool_use
// accumulator and emits an OpenAI tool_calls argument fragment.
// Unknown delta types are silently skipped.
func (s *stream) handleContentBlockDelta(event *anthropicStreamEvent) (bool, *llmtypes.Event, error) {
	switch event.Delta.Type {
	case "text_delta":
		return true, s.buildDeltaEvent(llmtypes.Delta{Content: event.Delta.Text}), nil
	case "thinking_delta":
		thinking := event.Delta.Thinking
		if thinking == "" {
			thinking = event.Delta.Text
		}
		return true, s.buildDeltaEvent(llmtypes.Delta{ReasoningContent: thinking}), nil
	case "input_json_delta":
		return s.handleInputJSONDelta(event)
	default:
		return false, nil, nil
	}
}

// buildDeltaEvent wraps a llmtypes.Delta into the surrounding chunk
// envelope (id, model, single choice). RecordEmit is called as part of
// every emitted chunk so audit chunk counts stay accurate.
func (s *stream) buildDeltaEvent(delta llmtypes.Delta) *llmtypes.Event {
	s.RecordEmit()
	return &llmtypes.Event{
		ID:     s.msgID,
		Object: "chat.completion.chunk",
		Model:  s.msgModel,
		Choices: []llmtypes.ChoiceDelta{{
			Index: 0,
			Delta: delta,
		}},
	}
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
func (s *stream) handleMessageStop() *llmtypes.Event {
	if s.pendingFinish == nil {
		s.pendingFinish = &anthropicEnd{finishReason: "stop"}
	}
	s.pendingEmitted = true
	s.RecordEmit()
	return s.buildFinishEvent(s.pendingFinish)
}

// finalize handles the post-loop state. Transport errors are already
// bubbled up by the SSE reader during the loop. If we have a buffered
// finish but never saw message_stop, surface it as the final chunk —
// otherwise treat the abrupt clean-EOF as an upstream fault (Anthropic
// must terminate with message_stop).
func (s *stream) finalize() (*llmtypes.Event, error) {
	if s.pendingFinish != nil && !s.pendingEmitted {
		return s.emitFinish(), nil
	}
	return nil, &llmtypes.Error{
		Kind:     llmtypes.KindUpstream,
		Provider: s.ProviderName,
		Message:  "stream ended without message_stop",
	}
}

func (s *stream) Summary() *llmtypes.Summary {
	summary := &llmtypes.Summary{
		Model:       s.msgModel,
		ChunkCount:  s.ChunkCount,
		FirstByteAt: s.FirstByteAt,
	}
	if s.pendingFinish != nil {
		summary.FinishReason = s.pendingFinish.finishReason
		usage := &llmtypes.Usage{
			PromptTokens:     s.inputTokens,
			CompletionTokens: s.pendingFinish.outputTokens,
			TotalTokens:      s.inputTokens + s.pendingFinish.outputTokens,
		}
		addCacheUsageExtra(usage, s.pendingFinish.cacheCreationTokens, s.pendingFinish.cacheReadTokens)
		summary.Usage = usage
	} else if s.inputTokens > 0 {
		// Partial: only message_start arrived. Surface what we got so audit
		// can record prompt token consumption even when generation aborted.
		summary.Usage = &llmtypes.Usage{
			PromptTokens: s.inputTokens,
			TotalTokens:  s.inputTokens,
		}
	}
	return summary
}

func (s *stream) buildFinishEvent(end *anthropicEnd) *llmtypes.Event {
	if end == nil {
		end = &anthropicEnd{finishReason: "stop"}
	}
	usage := &llmtypes.Usage{
		PromptTokens:     s.inputTokens,
		CompletionTokens: end.outputTokens,
		TotalTokens:      s.inputTokens + end.outputTokens,
	}
	addCacheUsageExtra(usage, end.cacheCreationTokens, end.cacheReadTokens)
	return &llmtypes.Event{
		ID:     s.msgID,
		Object: "chat.completion.chunk",
		Model:  s.msgModel,
		Choices: []llmtypes.ChoiceDelta{{
			Index:        0,
			Delta:        llmtypes.Delta{},
			FinishReason: end.finishReason,
		}},
		Usage: usage,
	}
}

type anthropicStreamEvent struct {
	Type         string             `json:"type"`
	Index        int                `json:"index,omitempty"`
	Message      *anthropicResponse `json:"message,omitempty"`
	ContentBlock *anthropicContent  `json:"content_block,omitempty"`
	Delta        struct {
		Type        string  `json:"type"`
		Text        string  `json:"text,omitempty"`
		Thinking    string  `json:"thinking,omitempty"`
		PartialJSON string  `json:"partial_json,omitempty"`
		StopReason  *string `json:"stop_reason"`
	} `json:"delta"`
	Usage anthropicUsage `json:"usage"`
}

func parseMaybeStreamError(payload []byte, providerName string) *llmtypes.Error {
	message, _ := envelopeMessage(payload)
	if message == "" {
		return nil
	}
	return errorFromStreamEvent(payload, providerName)
}
