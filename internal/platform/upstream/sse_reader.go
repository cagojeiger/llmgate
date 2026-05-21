package upstream

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"

	"llmgate/internal/domain/llmtypes"
)

// SSE wire constants. Kept as package-level []byte literals so the
// scanner loop can compare and trim without re-allocating per line.
var (
	sseDataPrefix   = []byte("data:")
	sseSpacePrefix  = []byte{' '}
	sseDoneSentinel = []byte("[DONE]")
	sseNewline      = []byte{'\n'}
)

// StatusError carries a non-2xx HTTP response from an SSE stream-open
// attempt. The adapter classifies Status into a llmtypes.ErrorKind via
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
	resp, err := client.Do(req) //nolint:gosec // SSRF: req URL comes from operator-configured catalog endpoints, not user input
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
//     typed *llmtypes.Error so callers can audit the transport fault.
type SSEReader struct {
	scanner *bufio.Scanner
	done    bool
}

// SSE scanner buffer sizing. Reasoning-heavy LLM events (DeepSeek
// thinking chains, GPT-5 chain-of-thought) can exceed bufio.Scanner's
// 64 KiB default in a single SSE frame, so we provision a 1 MiB
// starting buffer and a 10 MiB hard cap.
const (
	sseScannerInitialBuf = 1 << 20      // 1 MiB
	sseScannerMaxBuf     = 10 * 1 << 20 // 10 MiB
)

// NewSSEReader constructs an SSE reader over r. The underlying scanner
// is sized to handle large single-line payloads (see
// sseScannerMaxBuf) since reasoning-heavy LLM events can exceed the
// default 64 KiB cap.
func NewSSEReader(r io.ReadCloser) *SSEReader {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, sseScannerInitialBuf), sseScannerMaxBuf)
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
		// scanner.Bytes() reuses an internal buffer across Scan() calls;
		// any slice we keep across iterations must be copied (see
		// bytes.Clone below).
		line := r.scanner.Bytes()
		if len(line) == 0 {
			if len(parts) == 0 {
				continue
			}
			payload := bytes.Join(parts, sseNewline)
			if bytes.Equal(payload, sseDoneSentinel) {
				r.done = true
				return nil, io.EOF
			}
			return payload, nil
		}
		// Skip comments (`:` prefix) and non-data fields (`event:`,
		// `id:`, `retry:`). The SSE spec treats unknown fields as
		// ignorable.
		if !bytes.HasPrefix(line, sseDataPrefix) {
			continue
		}
		line = bytes.TrimPrefix(line, sseDataPrefix)
		line = bytes.TrimPrefix(line, sseSpacePrefix)
		parts = append(parts, bytes.Clone(line))
	}

	if err := r.scanner.Err(); err != nil {
		// Even if `parts` is non-empty we intentionally do not flush
		// them — partial bytes received before a connection drop may
		// themselves be corrupt, so the error signal is the priority
		// and any salvage attempt belongs to the adapter that knows
		// how to validate the JSON shape.
		//
		// scanner.Err() returns one of two flavors:
		//   1. An error bubbled up from the underlying io.Reader (the
		//      live HTTP connection): TCP/TLS read failure, peer
		//      reset, deadline exceeded. Message can carry upstream
		//      IPs / hostnames / ports.
		//   2. A scanner-internal error (e.g. bufio.ErrTooLong when an
		//      SSE frame exceeds the 10 MiB buffer cap). Message is a
		//      bufio diagnostic with no transport detail and is
		//      operationally useful to surface unchanged.
		//
		// Classify (1) as KindNetwork / KindTimeout so server/errors.go's
		// transport-class collapse strips the cause detail before it
		// reaches the wire. Default to KindUpstream for (2) so the
		// diagnostic survives intact and fallback / circuit-breaker
		// policy doesn't mistake a frame-size error for a
		// connectivity failure.
		kind := llmtypes.KindUpstream
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			kind = llmtypes.KindTimeout
		default:
			var netErr net.Error
			if errors.As(err, &netErr) {
				if netErr.Timeout() {
					kind = llmtypes.KindTimeout
				} else {
					kind = llmtypes.KindNetwork
				}
			}
		}
		return nil, &llmtypes.Error{Kind: kind, Message: err.Error(), Cause: err}
	}
	// Natural EOF (clean stream end). If parts were buffered (e.g.
	// upstream finished delivering `data: ...` lines but the trailing
	// blank line was elided), surface them as one last event before
	// reporting EOF — losing the buffered payload would mask data the
	// caller successfully received.
	if len(parts) > 0 {
		r.done = true
		payload := bytes.Join(parts, sseNewline)
		if bytes.Equal(payload, sseDoneSentinel) {
			return nil, io.EOF
		}
		return payload, nil
	}
	r.done = true
	return nil, io.EOF
}
