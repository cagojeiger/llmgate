package upstream

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"strings"

	"llmgate/internal/provider"
)

// StatusError carries a non-2xx HTTP response from an SSE stream-open
// attempt. The adapter classifies Status into a provider.Error.Kind via
// its vendor-specific envelope knowledge.
type StatusError struct {
	Status     int
	Body       []byte
	RetryAfter string
}

// OpenSSE sends req and validates the response is ready for streaming.
//
//   - Transport failure → err already stamped with providerName.
//   - Non-2xx status → *StatusError with body fully drained for the
//     adapter to classify. resp.Body has been closed.
//   - Success → *http.Response with body open; caller owns Close.
//
// Adapters use this so the four-step send → status-check → drain dance
// lives in one place.
func OpenSSE(client *http.Client, req *http.Request, providerName string) (*http.Response, *StatusError, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, LowLevelError(providerName, "send request", err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		raw, ioErr := io.ReadAll(resp.Body)
		if ioErr != nil {
			return nil, nil, LowLevelError(providerName, "read error response", ioErr)
		}
		return nil, &StatusError{
			Status:     resp.StatusCode,
			Body:       raw,
			RetryAfter: resp.Header.Get("Retry-After"),
		}, nil
	}
	return resp, nil, nil
}

// SSEReader pulls one server-sent event payload at a time from an
// upstream provider's response stream. Each Recv returns the next
// event's `data:` content (multi-line payloads are concatenated with
// `\n` per the SSE spec). Non-data fields (`event:`, `id:`, `retry:`)
// and `:` comment lines are accepted but discarded — adapters that
// need event types should rely on the JSON body shape instead.
//
// Termination contract:
//   - The OpenAI sentinel `[DONE]` is consumed and surfaced as io.EOF
//     so OpenAI-compatible streams end cleanly.
//   - A natural EOF (scanner exhausted with no buffered event) also
//     returns io.EOF — Anthropic doesn't emit `[DONE]` and ends the
//     stream after the final `message_stop` event.
//   - A scanner error (mid-stream cut, body read failure) returns a
//     typed *provider.Error so callers can audit the transport fault.
type SSEReader struct {
	scanner *bufio.Scanner
	done    bool
}

// NewSSEReader constructs an SSE reader over r. The underlying scanner
// is sized to handle large single-line payloads (up to 10 MiB) since
// reasoning-heavy LLM events can exceed the default 64 KiB cap.
func NewSSEReader(r io.ReadCloser) *SSEReader {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)
	return &SSEReader{scanner: scanner}
}

// Recv returns the next event's data payload, io.EOF when the stream
// has terminated cleanly, or a typed transport error.
func (r *SSEReader) Recv() (data []byte, err error) {
	if r.done {
		return nil, io.EOF
	}

	var parts [][]byte
	for r.scanner.Scan() {
		line := r.scanner.Text()
		if line == "" {
			if len(parts) == 0 {
				continue
			}
			payload := bytes.Join(parts, []byte("\n"))
			if string(payload) == "[DONE]" {
				r.done = true
				return nil, io.EOF
			}
			return payload, nil
		}
		// Skip comments (`:` prefix) and non-data fields (`event:`,
		// `id:`, `retry:`). The SSE spec treats unknown fields as
		// ignorable.
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
		parts = append(parts, []byte(payload))
	}

	if err := r.scanner.Err(); err != nil {
		// Transport-level fault. Even if `parts` is non-empty we
		// intentionally do not flush them — partial bytes received
		// before a connection drop may themselves be corrupt, so the
		// error signal is the priority and any salvage attempt belongs
		// to the adapter that knows how to validate the JSON shape.
		return nil, &provider.Error{Kind: provider.KindUpstream, Message: err.Error(), Cause: err}
	}
	// Natural EOF (clean stream end). If parts were buffered (e.g.
	// upstream finished delivering `data: ...` lines but the trailing
	// blank line was elided), surface them as one last event before
	// reporting EOF — losing the buffered payload would mask data the
	// caller successfully received.
	if len(parts) > 0 {
		r.done = true
		payload := bytes.Join(parts, []byte("\n"))
		if string(payload) == "[DONE]" {
			return nil, io.EOF
		}
		return payload, nil
	}
	r.done = true
	return nil, io.EOF
}
