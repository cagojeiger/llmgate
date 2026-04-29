package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"llmgate/internal/provider"
)

const (
	defaultBaseURL = "https://opencode.ai/zen/go/v1"
	userAgent      = "llmgate/0.1"
)

type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

type Option func(*Client)

func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") }
}

func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

func New(apiKey string, opts ...Option) *Client {
	c := &Client{
		baseURL: defaultBaseURL,
		apiKey:  apiKey,
		http: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   50,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *Client) Name() string { return "opencode" }

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

func (c *Client) CompleteStream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	if err := req.Validate(); err != nil {
		return nil, withProvider(err)
	}

	body, err := requestBodyWithStream(req)
	if err != nil {
		return nil, badRequest("marshal request", err, nil)
	}

	httpReq, err := c.newRequest(ctx, "text/event-stream", body)
	if err != nil {
		return nil, badRequest("build request", err, nil)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, lowLevelError("send request", err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, lowLevelError("read error response", err)
		}
		return nil, classify(resp.StatusCode, raw, resp.Header.Get("Retry-After"))
	}

	return &stream{
		body:   resp.Body,
		reader: provider.NewSSEReader(resp.Body),
	}, nil
}

func (c *Client) newRequest(ctx context.Context, accept string, body []byte) (*http.Request, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", accept)
	httpReq.Header.Set("User-Agent", userAgent)
	return httpReq, nil
}

type stream struct {
	body   io.Closer
	reader *provider.SSEReader
}

func (s *stream) Recv() (*provider.Event, error) {
	data, err := s.reader.Recv()
	if err != nil {
		return nil, withProvider(err)
	}
	if perr := parseStreamError(data); perr != nil {
		return nil, perr
	}

	var event provider.Event
	if err := json.Unmarshal(data, &event); err != nil {
		return nil, &provider.Error{
			Kind:     provider.KindUpstream,
			Provider: "opencode",
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

func classify(status int, body []byte, retryAfterHeader string) *provider.Error {
	message := envelopeMessage(body)
	if message == "" {
		if len(body) > 0 {
			message = fmt.Sprintf("upstream returned status %d: %s", status, string(firstBytes(body)))
		} else {
			message = fmt.Sprintf("upstream returned status %d", status)
		}
	}

	kind := provider.KindUnknown
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		kind = provider.KindAuth
	case status == http.StatusNotFound:
		kind = provider.KindBadRequest
	case status == http.StatusBadRequest, status == http.StatusUnprocessableEntity:
		kind = provider.KindBadRequest
		lower := strings.ToLower(message)
		if strings.Contains(lower, "token limit") || strings.Contains(lower, "context length") {
			kind = provider.KindContextLength
		}
	case status == http.StatusTooManyRequests:
		kind = provider.KindRateLimit
	case status == 529, status >= 500 && status <= 599:
		kind = provider.KindUpstream
	}

	return &provider.Error{
		Kind:       kind,
		Provider:   "opencode",
		Message:    message,
		StatusCode: status,
		RetryAfter: parseRetryAfter(retryAfterHeader),
		Raw:        firstBytes(body),
	}
}

func envelopeMessage(body []byte) string {
	var env struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	return env.Error.Message
}

func parseStreamError(data []byte) *provider.Error {
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
		Provider: "opencode",
		Message:  env.Error.Message,
		Raw:      firstBytes(data),
	}
}

func kindFromErrorType(errorType, message string) provider.Kind {
	lowerType := strings.ToLower(errorType)
	lowerMessage := strings.ToLower(message)
	switch {
	case strings.Contains(lowerType, "auth"):
		return provider.KindAuth
	case strings.Contains(lowerType, "rate"):
		return provider.KindRateLimit
	case strings.Contains(lowerType, "context") ||
		strings.Contains(lowerMessage, "token limit") ||
		strings.Contains(lowerMessage, "context length"):
		return provider.KindContextLength
	case strings.Contains(lowerType, "content_filter"):
		return provider.KindContentFilter
	case strings.Contains(lowerType, "invalid"):
		return provider.KindBadRequest
	case strings.Contains(lowerType, "upstream"):
		return provider.KindUpstream
	}
	return provider.KindUpstream
}

func parseRetryAfter(header string) time.Duration {
	if header == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(header); err == nil {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(header); err == nil {
		d := time.Until(at)
		if d > 0 {
			return d
		}
	}
	return 0
}

func lowLevelError(message string, cause error) *provider.Error {
	kind := provider.KindNetwork
	if errors.Is(cause, context.DeadlineExceeded) {
		kind = provider.KindTimeout
	} else {
		var netErr net.Error
		if errors.As(cause, &netErr) && netErr.Timeout() {
			kind = provider.KindTimeout
		}
	}
	return &provider.Error{
		Kind:     kind,
		Provider: "opencode",
		Message:  message + ": " + cause.Error(),
		Cause:    cause,
	}
}

func badRequest(message string, cause error, raw []byte) *provider.Error {
	return &provider.Error{
		Kind:     provider.KindBadRequest,
		Provider: "opencode",
		Message:  message + ": " + cause.Error(),
		Cause:    cause,
		Raw:      firstBytes(raw),
	}
}

func withProvider(err error) error {
	var perr *provider.Error
	if !errors.As(err, &perr) {
		return err
	}
	if perr.Provider == "opencode" {
		return perr
	}
	copy := *perr
	copy.Provider = "opencode"
	return &copy
}

func firstBytes(b []byte) []byte {
	if len(b) > 256 {
		b = b[:256]
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
