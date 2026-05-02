package openai

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"

	"llmgate/internal/provider"
)

func (c *Client) CompleteStream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	if err := req.Validate(); err != nil {
		return nil, provider.StampProvider(err, c.cfg.Name)
	}

	body, err := requestBodyWithStream(req)
	if err != nil {
		return nil, c.badRequest("marshal request", err, nil)
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

	return &stream{
		body:         resp.Body,
		reader:       provider.NewSSEReader(resp.Body),
		providerName: c.cfg.Name,
	}, nil
}

type stream struct {
	body         io.Closer
	reader       *provider.SSEReader
	providerName string
	closeOnce    sync.Once
	closeErr     error

	// accumulated state for Summary()
	model        string
	finishReason string
	usage        *provider.Usage
	vendorCost   string
	chunkCount   int
	firstByteAt  time.Time
}

func (s *stream) Recv() (*provider.Event, error) {
	data, err := s.reader.Recv()
	if err != nil {
		return nil, provider.StampProvider(err, s.providerName)
	}
	if perr := parseStreamError(data, s.providerName); perr != nil {
		return nil, perr
	}

	var event provider.Event
	if err := json.Unmarshal(data, &event); err != nil {
		return nil, &provider.Error{
			Kind:     provider.KindUpstream,
			Provider: s.providerName,
			Message:  "decode stream event: " + err.Error(),
			Cause:    err,
			Raw:      firstBytes(data),
		}
	}

	if s.firstByteAt.IsZero() {
		s.firstByteAt = time.Now()
	}
	s.chunkCount++
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

func (s *stream) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.body.Close()
	})
	return s.closeErr
}

func (s *stream) Summary() *provider.Summary {
	return &provider.Summary{
		Model:        s.model,
		FinishReason: s.finishReason,
		Usage:        s.usage,
		VendorCost:   s.vendorCost,
		ChunkCount:   s.chunkCount,
		FirstByteAt:  s.firstByteAt,
	}
}

// requestBodyWithStream marshals a copy of req with Stream forced true
// so callers don't need to set the flag themselves.
func requestBodyWithStream(req *provider.Request) ([]byte, error) {
	cp := *req
	t := true
	cp.Stream = &t
	return json.Marshal(&cp)
}

// parseStreamError returns a *provider.Error if the SSE event payload is
// an upstream error envelope (mid-stream surfaced as data: {"error":...}).
// Heuristic kind detection: error.type or message string match — best
// effort, default KindUpstream when no token wins.
func parseStreamError(data []byte, providerName string) *provider.Error {
	var env struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &env); err != nil || env.Error.Message == "" {
		return nil
	}
	t := strings.ToLower(env.Error.Type)
	m := strings.ToLower(env.Error.Message)
	kind := provider.KindUpstream
	switch {
	case strings.Contains(t, "auth"):
		kind = provider.KindAuth
	case strings.Contains(t, "rate"):
		kind = provider.KindRateLimit
	case strings.Contains(t, "context") || strings.Contains(m, "token limit") || strings.Contains(m, "context length"):
		kind = provider.KindContextLength
	case strings.Contains(t, "content_filter"):
		kind = provider.KindContentFilter
	case strings.Contains(t, "invalid"):
		kind = provider.KindBadRequest
	}
	return &provider.Error{
		Kind:     kind,
		Provider: providerName,
		Message:  env.Error.Message,
		Raw:      firstBytes(data),
	}
}
