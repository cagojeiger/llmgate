package anthropic

import (
	"encoding/json"

	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/platform/upstream"
)

type streamEventResult struct {
	event *llmtypes.Event
	err   error
}

func emitStreamEvent(event *llmtypes.Event) streamEventResult {
	return streamEventResult{event: event}
}

func skipStreamEvent() streamEventResult {
	return streamEventResult{}
}

func failStreamEvent(err error) streamEventResult {
	return streamEventResult{err: err}
}

func decodeStreamPayload(payload []byte, providerName string) (*anthropicStreamEvent, error) {
	var event anthropicStreamEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, &llmtypes.Error{
			Kind:     llmtypes.KindUpstream,
			Provider: providerName,
			Message:  "upstream returned invalid response",
			Cause:    err,
			Raw:      upstream.FirstBytes(payload),
		}
	}
	return &event, nil
}

func (s *stream) dispatch(event *anthropicStreamEvent, payload []byte) streamEventResult {
	switch event.Type {
	case "message_start":
		return emitStreamEvent(s.handleMessageStart(event))
	case "content_block_start":
		return s.handleContentBlockStart(event)
	case "content_block_delta":
		return s.handleContentBlockDelta(event)
	case "content_block_stop":
		return s.handleContentBlockStop(event)
	case "message_delta":
		s.handleMessageDelta(event)
		return skipStreamEvent()
	case "message_stop":
		return emitStreamEvent(s.handleMessageStop())
	case "ping":
		return s.handlePing(event)
	case "error":
		return failStreamEvent(errorFromStreamEvent(payload, s.ProviderName))
	default:
		if perr := parseMaybeStreamError(payload, s.ProviderName); perr != nil {
			return failStreamEvent(perr)
		}
		return skipStreamEvent()
	}
}

func (s *stream) handlePing(event *anthropicStreamEvent) streamEventResult {
	if len(event.Cost) == 0 {
		return skipStreamEvent()
	}
	s.vendorCost = append(json.RawMessage(nil), event.Cost...)
	if s.pendingFinish == nil {
		return skipStreamEvent()
	}

	usage := s.buildUsage(s.pendingFinish)
	source := llmtypes.AttachUsageCost(usage, s.vendorCost, s.cost)
	if source == llmtypes.UsageCostNone || source == s.costSource || s.costSource == llmtypes.UsageCostProvider {
		return skipStreamEvent()
	}
	s.costSource = source
	s.RecordEmit()
	return emitStreamEvent(&llmtypes.Event{
		ID:      s.msgID,
		Object:  "chat.completion.chunk",
		Model:   s.msgModel,
		Choices: []llmtypes.ChoiceDelta{},
		Usage:   usage,
		Extra:   map[string]json.RawMessage{"cost": append(json.RawMessage(nil), event.Cost...)},
	})
}

func (s *stream) handleMessageStart(event *anthropicStreamEvent) *llmtypes.Event {
	if event.Message != nil {
		s.msgID = event.Message.ID
		s.msgModel = event.Message.Model
		s.inputTokens = event.Message.Usage.InputTokens
		s.cacheCreationTokens = event.Message.Usage.CacheCreationInputTokens
		s.cacheReadTokens = event.Message.Usage.CacheReadInputTokens
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

func (s *stream) handleContentBlockDelta(event *anthropicStreamEvent) streamEventResult {
	switch event.Delta.Type {
	case "text_delta":
		return emitStreamEvent(s.buildDeltaEvent(llmtypes.Delta{Content: event.Delta.Text}))
	case "thinking_delta":
		thinking := event.Delta.Thinking
		if thinking == "" {
			thinking = event.Delta.Text
		}
		return emitStreamEvent(s.buildDeltaEvent(llmtypes.Delta{ReasoningContent: thinking}))
	case "input_json_delta":
		return s.handleInputJSONDelta(event)
	default:
		return skipStreamEvent()
	}
}

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

func (s *stream) handleMessageDelta(event *anthropicStreamEvent) {
	finishReason := ""
	if event.Delta.StopReason != nil {
		finishReason = mapStopReason(*event.Delta.StopReason)
	}
	cacheCreationTokens := event.Usage.CacheCreationInputTokens
	if cacheCreationTokens == 0 {
		cacheCreationTokens = s.cacheCreationTokens
	}
	cacheReadTokens := event.Usage.CacheReadInputTokens
	if cacheReadTokens == 0 {
		cacheReadTokens = s.cacheReadTokens
	}
	s.pendingFinish = &anthropicEnd{
		finishReason:        finishReason,
		outputTokens:        event.Usage.OutputTokens,
		cacheCreationTokens: cacheCreationTokens,
		cacheReadTokens:     cacheReadTokens,
	}
}

func (s *stream) handleMessageStop() *llmtypes.Event {
	if s.pendingFinish == nil {
		s.pendingFinish = &anthropicEnd{finishReason: "stop"}
	}
	s.pendingEmitted = true
	s.RecordEmit()
	return s.buildFinishEvent(s.pendingFinish)
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
	Usage anthropicUsage  `json:"usage"`
	Cost  json.RawMessage `json:"cost,omitempty"`
}

func parseMaybeStreamError(payload []byte, providerName string) *llmtypes.Error {
	message, _ := envelopeMessage(payload)
	if message == "" {
		return nil
	}
	return errorFromStreamEvent(payload, providerName)
}
