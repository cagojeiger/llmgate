package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync/atomic"

	"llmgate/internal/llmtypes"
	"llmgate/internal/streaming"
	"llmgate/internal/upstream"
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

// streamToolCallState tracks the state of one Anthropic tool_use block
// while OpenAI tool_calls deltas are being emitted for it. Anthropic
// argument fragments are pass-through — each input_json_delta is
// converted to an OpenAI arguments delta and forwarded immediately, so
// no accumulator is needed here. Started records whether the *first*
// delta (which carries id + name) has already been emitted; subsequent
// deltas omit id/name and only extend arguments. Placeholder marks the
// case where Anthropic begins the block with input "{}" and intends to
// send the real arguments in later input_json_delta events — when no
// deltas arrive (zero-arg tool) we emit the empty object on
// content_block_stop.
type streamToolCallState struct {
	ID          string
	Name        string
	Index       int
	Started     bool
	Placeholder bool
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

// handleContentBlockStart opens a tool_use accumulator when Anthropic
// begins a content block of type tool_use, and emits the first OpenAI
// tool_calls fragment carrying id + function.name. Other block types
// (text / thinking) need no preamble in the OpenAI wire — the deltas
// themselves carry everything callers expect.
func (s *stream) handleContentBlockStart(event *anthropicStreamEvent) (bool, *llmtypes.Event, error) {
	if event.ContentBlock == nil || event.ContentBlock.Type != "tool_use" {
		return false, nil, nil
	}
	state := &streamToolCallState{
		ID:    event.ContentBlock.ID,
		Name:  event.ContentBlock.Name,
		Index: s.nextToolCallIndex,
	}
	s.nextToolCallIndex++

	initial := initialToolArguments(event.ContentBlock.Input)
	state.Placeholder = initial == "{}"
	if state.Placeholder {
		// Wait for input_json_delta events to fill in the real args; if
		// none arrive (zero-arg tool) content_block_stop will flush "{}".
		s.toolCalls[event.Index] = state
		return false, nil, nil
	}
	state.Started = true
	s.toolCalls[event.Index] = state
	return true, s.buildDeltaEvent(buildToolCallStartDelta(state, initial)), nil
}

// handleInputJSONDelta is the body of content_block_delta when Anthropic
// is incrementally streaming a tool_use block's JSON input. The first
// delta also carries the id + name (because content_block_start used a
// placeholder); subsequent deltas only extend arguments.
func (s *stream) handleInputJSONDelta(event *anthropicStreamEvent) (bool, *llmtypes.Event, error) {
	if event.Delta.PartialJSON == "" {
		return false, nil, nil
	}
	state, ok := s.toolCalls[event.Index]
	if !ok || state == nil {
		return false, nil, nil
	}
	if state.Placeholder {
		state.Placeholder = false
	}
	if !state.Started {
		state.Started = true
		return true, s.buildDeltaEvent(buildToolCallStartDelta(state, event.Delta.PartialJSON)), nil
	}
	return true, s.buildDeltaEvent(buildToolCallArgsDelta(state.Index, event.Delta.PartialJSON)), nil
}

// handleContentBlockStop flushes the deferred placeholder case for a
// zero-argument tool (content_block_start saw input "{}", no
// input_json_delta events arrived, content_block_stop now closes the
// block). Other paths have already emitted their deltas.
func (s *stream) handleContentBlockStop(event *anthropicStreamEvent) (bool, *llmtypes.Event, error) {
	state, ok := s.toolCalls[event.Index]
	if !ok || state == nil || state.Started {
		return false, nil, nil
	}
	if !state.Placeholder {
		return false, nil, nil
	}
	state.Started = true
	return true, s.buildDeltaEvent(buildToolCallStartDelta(state, "{}")), nil
}

// initialToolArguments returns the trimmed JSON form of an Anthropic
// content_block_start input field. An empty input (no field on the
// wire) returns ""; a present-but-empty object returns "{}" so the
// caller can detect the placeholder case.
func initialToolArguments(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	return strings.TrimSpace(string(input))
}

// buildToolCallStartDelta builds the *first* OpenAI tool_calls fragment
// for a tool call: includes the openai-allocated index, id, type, and
// function name plus the initial arguments fragment.
func buildToolCallStartDelta(state *streamToolCallState, args string) llmtypes.Delta {
	return toolCallDelta([]map[string]any{{
		"index": state.Index,
		"id":    state.ID,
		"type":  "function",
		"function": map[string]any{
			"name":      state.Name,
			"arguments": args,
		},
	}})
}

// buildToolCallArgsDelta builds a continuation tool_calls fragment
// (no id / name) extending an in-flight call's arguments.
func buildToolCallArgsDelta(index int, args string) llmtypes.Delta {
	return toolCallDelta([]map[string]any{{
		"index": index,
		"function": map[string]any{
			"arguments": args,
		},
	}})
}

func toolCallDelta(toolCalls []map[string]any) llmtypes.Delta {
	raw, _ := json.Marshal(toolCalls)
	return llmtypes.Delta{
		Extra: map[string]json.RawMessage{"tool_calls": raw},
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
