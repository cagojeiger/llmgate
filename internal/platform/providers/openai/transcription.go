package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/platform/upstream"
)

// TranscriptionConfig configures a TranscriptionClient. It mirrors the chat
// Config's transport knobs but makes auth optional: a local STT server is
// commonly unauthenticated, so an empty APIKey sends no Authorization header
// rather than failing construction the way the chat Client does.
type TranscriptionConfig struct {
	BaseURL    string
	APIKey     string // optional; empty means the upstream is unauthenticated
	AuthScheme string // bearer | x-api-key; only consulted when APIKey is set
	UserAgent  string
	HTTPClient *http.Client
	Name       string
}

// TranscriptionClient adapts an OpenAI-compatible speech-to-text upstream to
// llmtypes.TranscriptionProvider. It POSTs multipart/form-data to
// BaseURL + "/audio/transcriptions" (BaseURL is the /v1 API root, same
// convention as chat) and parses the result into a typed
// TranscriptionResponse so the full result can be audited, not passed through
// opaquely.
type TranscriptionClient struct {
	cfg  TranscriptionConfig
	http *http.Client
}

func NewTranscription(cfg TranscriptionConfig) (*TranscriptionClient, error) {
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.BaseURL == "" {
		return nil, errors.New("openai: BaseURL is required")
	}
	cfg.AuthScheme = strings.ToLower(cfg.AuthScheme)
	// Only validate the scheme when a key is present — an unauthenticated
	// upstream legitimately carries neither key nor scheme.
	if cfg.APIKey != "" {
		if cfg.AuthScheme == "" {
			cfg.AuthScheme = "bearer"
		}
		if cfg.AuthScheme != "bearer" && cfg.AuthScheme != "x-api-key" {
			return nil, fmt.Errorf("openai: unsupported AuthScheme %q", cfg.AuthScheme)
		}
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = defaultUserAgent
	}
	if cfg.Name == "" {
		cfg.Name = "openai"
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = upstream.DefaultClient()
	}
	return &TranscriptionClient{cfg: cfg, http: httpClient}, nil
}

func (c *TranscriptionClient) Name() string { return c.cfg.Name }

func (c *TranscriptionClient) Transcribe(ctx context.Context, req *llmtypes.TranscriptionRequest) (*llmtypes.TranscriptionResponse, error) {
	if err := req.Validate(); err != nil {
		return nil, llmtypes.StampProvider(err, c.cfg.Name)
	}

	body, contentType, err := c.buildMultipart(req)
	if err != nil {
		return nil, c.badRequest("build multipart body", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/audio/transcriptions", bytes.NewReader(body))
	if err != nil {
		return nil, c.badRequest("build request", err)
	}
	httpReq.Header.Set("Content-Type", contentType)
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", c.cfg.UserAgent)
	if c.cfg.APIKey != "" {
		switch c.cfg.AuthScheme {
		case "x-api-key":
			httpReq.Header.Set("X-Api-Key", c.cfg.APIKey)
		default:
			httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
		}
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, upstream.LowLevelError(c.cfg.Name, "send request", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, upstream.LowLevelError(c.cfg.Name, "read response", err)
	}

	if resp.StatusCode >= 400 {
		return nil, classifyError(c.cfg.Name, resp.StatusCode, raw, resp.Header.Get("Retry-After"))
	}
	if len(raw) == 0 {
		return nil, &llmtypes.Error{Kind: llmtypes.KindEmpty, Provider: c.cfg.Name, Message: "empty response"}
	}

	return c.parseResponse(req.ResponseFormat, raw)
}

// buildMultipart encodes the request as multipart/form-data with the OpenAI
// field names. The audio bytes go in the "file" part with the original
// filename preserved so the upstream can sniff the container format.
func (c *TranscriptionClient) buildMultipart(req *llmtypes.TranscriptionRequest) ([]byte, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	filename := req.Filename
	if filename == "" {
		filename = "audio"
	}
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(req.Audio); err != nil {
		return nil, "", err
	}

	fields := [][2]string{
		{"model", req.Model},
		{"language", req.Language},
		{"prompt", req.Prompt},
		{"response_format", req.ResponseFormat},
	}
	for _, f := range fields {
		if f[1] == "" {
			continue
		}
		if err := mw.WriteField(f[0], f[1]); err != nil {
			return nil, "", err
		}
	}
	if req.Temperature != nil {
		if err := mw.WriteField("temperature", strconv.FormatFloat(*req.Temperature, 'f', -1, 64)); err != nil {
			return nil, "", err
		}
	}
	// Opt the upstream into SSE. Only sent on the streaming path (req.Stream
	// forced true by TranscribeStream) so the non-stream body is unchanged.
	if req.Stream != nil && *req.Stream {
		if err := mw.WriteField("stream", "true"); err != nil {
			return nil, "", err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

// parseResponse turns the upstream body into a typed TranscriptionResponse.
// json / verbose_json return a JSON object; text / srt / vtt return a raw
// string body, which we wrap as the Text field so callers get one shape.
func (c *TranscriptionClient) parseResponse(responseFormat string, raw []byte) (*llmtypes.TranscriptionResponse, error) {
	switch strings.ToLower(responseFormat) {
	case "text", "srt", "vtt":
		return &llmtypes.TranscriptionResponse{Text: string(raw)}, nil
	default: // "", json, verbose_json
		var out llmtypes.TranscriptionResponse
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
}

func (c *TranscriptionClient) badRequest(message string, cause error) *llmtypes.Error {
	return upstream.BadRequest(c.cfg.Name, message, cause, nil)
}
