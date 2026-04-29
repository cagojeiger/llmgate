package openai

import (
	"context"
	"encoding/json"
	"io"

	"llmgate/internal/provider"
)

func (c *Client) CompleteStream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	if err := req.Validate(); err != nil {
		return nil, stampProvider(err, c.name)
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
		providerName: c.name,
	}, nil
}

type stream struct {
	body         io.Closer
	reader       *provider.SSEReader
	providerName string
}

func (s *stream) Recv() (*provider.Event, error) {
	data, err := s.reader.Recv()
	if err != nil {
		return nil, stampProvider(err, s.providerName)
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
	return &event, nil
}

func (s *stream) Close() error {
	return s.body.Close()
}

// requestBodyWithStream marshals req then injects "stream":true at the top
// level so the upstream switches to SSE. We re-marshal via a map to keep
// the user's Extra fields and ordering otherwise unchanged.
func requestBodyWithStream(req *provider.Request) ([]byte, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	raw["stream"] = json.RawMessage("true")
	return json.Marshal(raw)
}

// parseStreamError returns a *provider.Error if the SSE event payload is
// an upstream error envelope (mid-stream surfaced as data: {"error":...}).
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
	return &provider.Error{
		Kind:     kindFromErrorType(env.Error.Type, env.Error.Message),
		Provider: providerName,
		Message:  env.Error.Message,
		Raw:      firstBytes(data),
	}
}
