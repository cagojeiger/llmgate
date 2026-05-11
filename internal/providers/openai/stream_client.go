package openai

import (
	"context"
	"encoding/json"

	"llmgate/internal/llmtypes"
	"llmgate/internal/streaming"
	"llmgate/internal/upstream"
)

func (c *Client) CompleteStream(ctx context.Context, req *llmtypes.Request) (llmtypes.Stream, error) {
	if err := req.Validate(); err != nil {
		return nil, llmtypes.StampProvider(err, c.cfg.Name)
	}

	body, err := requestBodyWithStream(req)
	if err != nil {
		return nil, c.badRequest("marshal request", err, nil)
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
		reader: upstream.NewSSEReader(resp.Body),
	})
}

type stream struct {
	streaming.StreamBase

	reader *upstream.SSEReader

	// accumulated state for Summary()
	model        string
	finishReason string
	usage        *llmtypes.Usage
	vendorCost   string
}

func (s *stream) Recv() (*llmtypes.Event, error) {
	data, err := s.reader.Recv()
	if err != nil {
		return nil, llmtypes.StampProvider(err, s.ProviderName)
	}
	if perr := parseStreamError(data, s.ProviderName); perr != nil {
		return nil, perr
	}

	var event llmtypes.Event
	if err := json.Unmarshal(data, &event); err != nil {
		return nil, &llmtypes.Error{
			Kind:     llmtypes.KindUpstream,
			Provider: s.ProviderName,
			Message:  "upstream returned invalid response",
			Cause:    err,
			Raw:      upstream.FirstBytes(data),
		}
	}

	s.RecordEmit()
	if event.Model != "" {
		s.model = event.Model
	}
	if event.Usage != nil {
		s.usage = event.Usage
	}
	if cost, ok := event.Extra["cost"]; ok && len(cost) > 0 {
		s.vendorCost = string(cost)
	}
	if len(event.Choices) > 0 && event.Choices[0].FinishReason != "" {
		s.finishReason = event.Choices[0].FinishReason
	}

	return &event, nil
}

func (s *stream) Summary() *llmtypes.Summary {
	return &llmtypes.Summary{
		Model:        s.model,
		FinishReason: s.finishReason,
		Usage:        s.usage,
		VendorCost:   s.vendorCost,
		ChunkCount:   s.ChunkCount,
		FirstByteAt:  s.FirstByteAt,
	}
}

// requestBodyWithStream marshals a copy of req with Stream forced true
// so callers don't need to set the flag themselves.
func requestBodyWithStream(req *llmtypes.Request) ([]byte, error) {
	cp := *req
	t := true
	cp.Stream = &t
	return json.Marshal(&cp)
}

// parseStreamError returns a *llmtypes.Error if the SSE event payload is
// an upstream error envelope (mid-stream surfaced as data: {"error":...}).
// Heuristic kind detection: error.type or message string match — best
// effort, default KindUpstream when no token wins.
func parseStreamError(data []byte, providerName string) *llmtypes.Error {
	env := parseErrorEnvelope(data)
	if env.Message == "" {
		return nil
	}
	kind := kindFromOpenAIError(0, env)
	return &llmtypes.Error{
		Kind:     kind,
		Provider: providerName,
		Message:  upstream.PublicProviderMessage(kind, env.Message),
		Raw:      upstream.FirstBytes(data),
	}
}
