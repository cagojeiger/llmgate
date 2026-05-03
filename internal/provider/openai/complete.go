package openai

import (
	"context"
	"encoding/json"
	"io"

	"llmgate/internal/provider"
	"llmgate/internal/upstream"
)

func (c *Client) Complete(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	if err := req.Validate(); err != nil {
		return nil, provider.StampProvider(err, c.cfg.Name)
	}

	body, err := json.Marshal(req)
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
		return nil, &provider.Error{Kind: provider.KindEmpty, Provider: c.cfg.Name, Message: "empty response"}
	}

	var out provider.Response
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &provider.Error{
			Kind:     provider.KindUpstream,
			Provider: c.cfg.Name,
			Message:  "decode response: " + err.Error(),
			Cause:    err,
			Raw:      upstream.FirstBytes(raw),
		}
	}
	return &out, nil
}
