package anthropic

import (
	"context"
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

	resp, statusErr, err := upstream.OpenSSE(c.http, httpReq, c.cfg.Name) //nolint:bodyclose // resp.Body ownership transfers to StreamBase; closed via Stream.Close
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

		event, err := decodeStreamPayload(payload, s.ProviderName)
		if err != nil {
			return nil, err
		}

		result := s.dispatch(event, payload)
		if result.err != nil {
			return nil, result.err
		}
		if result.event != nil {
			return result.event, nil
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
