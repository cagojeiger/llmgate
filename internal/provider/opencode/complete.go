package opencode

import (
	"context"
	"encoding/json"
	"io"

	"llmgate/internal/provider"
)

func (c *Client) Complete(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	if err := req.Validate(); err != nil {
		return nil, withProvider(err)
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, badRequest("marshal request", err, nil)
	}

	httpReq, err := c.newRequest(ctx, "application/json", body)
	if err != nil {
		return nil, badRequest("build request", err, nil)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, lowLevelError("send request", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, lowLevelError("read response", err)
	}

	if resp.StatusCode >= 400 {
		return nil, classify(resp.StatusCode, raw, resp.Header.Get("Retry-After"))
	}
	if len(raw) == 0 {
		return nil, &provider.Error{Kind: provider.KindEmpty, Provider: "opencode", Message: "empty response"}
	}

	var out provider.Response
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &provider.Error{
			Kind:     provider.KindUpstream,
			Provider: "opencode",
			Message:  "decode response: " + err.Error(),
			Cause:    err,
			Raw:      firstBytes(raw),
		}
	}
	return &out, nil
}
