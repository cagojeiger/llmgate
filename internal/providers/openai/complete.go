package openai

import (
	"context"
	"encoding/json"
	"io"

	"llmgate/internal/llmtypes"
	"llmgate/internal/upstream"
)

func (c *Client) Complete(ctx context.Context, req *llmtypes.Request) (*llmtypes.Response, error) {
	if err := req.Validate(); err != nil {
		return nil, llmtypes.StampProvider(err, c.cfg.Name)
	}

	body, err := c.marshalRequest(req)
	if err != nil {
		return nil, c.badRequest("marshal request", err, nil)
	}

	httpReq, err := c.newRequest(ctx, "application/json", body)
	if err != nil {
		return nil, c.badRequest("build request", err, nil)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, c.lowLevelError("send request", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, c.lowLevelError("read response", err)
	}

	if resp.StatusCode >= 400 {
		return nil, c.classify(resp.StatusCode, raw, resp.Header.Get("Retry-After"))
	}
	if len(raw) == 0 {
		return nil, &llmtypes.Error{Kind: llmtypes.KindEmpty, Provider: c.cfg.Name, Message: "empty response"}
	}

	var out llmtypes.Response
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &llmtypes.Error{
			Kind:     llmtypes.KindUpstream,
			Provider: c.cfg.Name,
			Message:  "upstream returned invalid response",
			Cause:    err,
			Raw:      upstream.FirstBytes(raw),
		}
	}
	return &out, nil
}
