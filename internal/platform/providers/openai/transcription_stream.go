package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/streaming"
	"llmgate/internal/platform/upstream"
)

// TranscribeStream POSTs the multipart request with stream=true and returns a
// TranscriptionStream over the upstream SSE. It mirrors Client.CompleteStream:
// OpenSSE handles the send + status-check dance, and a non-2xx status is
// classified so the routing layer can fall back before any bytes reach the
// client.
func (c *TranscriptionClient) TranscribeStream(ctx context.Context, req *llmtypes.TranscriptionRequest) (llmtypes.TranscriptionStream, error) {
	if err := req.Validate(); err != nil {
		return nil, llmtypes.StampProvider(err, c.cfg.Name)
	}

	// Force the stream field on a copy so the caller's request is untouched
	// and buildMultipart emits stream=true.
	streamReq := *req
	streamTrue := true
	streamReq.Stream = &streamTrue

	body, contentType, err := c.buildMultipart(&streamReq)
	if err != nil {
		return nil, c.badRequest("build multipart body", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/v1/audio/transcriptions", bytes.NewReader(body))
	if err != nil {
		return nil, c.badRequest("build request", err)
	}
	httpReq.Header.Set("Content-Type", contentType)
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("User-Agent", c.cfg.UserAgent)
	if c.cfg.APIKey != "" {
		switch c.cfg.AuthScheme {
		case "x-api-key":
			httpReq.Header.Set("X-Api-Key", c.cfg.APIKey)
		default:
			httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
		}
	}

	resp, statusErr, err := upstream.OpenSSE(c.http, httpReq, c.cfg.Name) //nolint:bodyclose // resp.Body ownership transfers to StreamBase; closed via Stream.Close
	if err != nil {
		return nil, err
	}
	if statusErr != nil {
		return nil, classifyError(c.cfg.Name, statusErr.Status, statusErr.Body, statusErr.RetryAfter)
	}

	return &transcriptionStream{
		StreamBase: streaming.StreamBase{
			Body:         resp.Body,
			ProviderName: c.cfg.Name,
		},
		reader: upstream.NewSSEReader(resp.Body),
	}, nil
}

// transcriptionStream adapts an OpenAI-compatible STT SSE body to
// llmtypes.TranscriptionStream. It parses each `transcript.text.*` event for
// the caller's final-text assembly while preserving the raw payload for
// verbatim relay.
type transcriptionStream struct {
	streaming.StreamBase

	reader *upstream.SSEReader
}

func (s *transcriptionStream) Recv() (*llmtypes.TranscriptionEvent, error) {
	data, err := s.reader.Recv()
	if err != nil {
		return nil, llmtypes.StampProvider(err, s.ProviderName)
	}
	// A mid-stream error envelope (data: {"error":...}) surfaces as a typed
	// error, same as the chat stream path.
	if perr := parseStreamError(data, s.ProviderName); perr != nil {
		return nil, perr
	}

	var event llmtypes.TranscriptionEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return nil, &llmtypes.Error{
			Kind:     llmtypes.KindUpstream,
			Provider: s.ProviderName,
			Message:  "upstream returned invalid response",
			Cause:    err,
			Raw:      upstream.FirstBytes(data),
		}
	}
	// Keep the original payload so the relay forwards the exact upstream
	// frame; the SSE reader returns a fresh slice per event, so retaining it
	// is safe without an extra copy.
	event.Raw = data

	s.RecordEmit()
	return &event, nil
}

func (s *transcriptionStream) Summary() *llmtypes.Summary {
	return &llmtypes.Summary{
		ChunkCount:  s.ChunkCount,
		FirstByteAt: s.FirstByteAt,
	}
}
