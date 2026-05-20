package anthropic

import (
	"encoding/json"
	"strings"

	"llmgate/internal/domain/llmtypes"
)

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
