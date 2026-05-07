package upstream

import (
	"errors"
	"io"
	"strings"
	"testing"

	"llmgate/internal/llmtypes"
)

func TestSSEReader_NormalDoneTermination(t *testing.T) {
	reader := NewSSEReader(io.NopCloser(strings.NewReader(
		"data: one\n\n" +
			"data: two\n\n" +
			"data: three\n\n" +
			"data: [DONE]\n\n",
	)))

	for _, want := range []string{"one", "two", "three"} {
		got, err := reader.Recv()
		if err != nil {
			t.Fatalf("Recv() error = %v", err)
		}
		if string(got) != want {
			t.Fatalf("Recv() = %q, want %q", got, want)
		}
	}
	if _, err := reader.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("Recv() after DONE error = %v, want io.EOF", err)
	}
}

func TestSSEReader_IgnoresCommentsEventIDRetry(t *testing.T) {
	reader := NewSSEReader(io.NopCloser(strings.NewReader(
		": leading comment\n\n" +
			"event: message_start\n" +
			"id: 123\n" +
			"retry: 5000\n" +
			"data: ok\n\n" +
			"data: [DONE]\n\n",
	)))

	got, err := reader.Recv()
	if err != nil {
		t.Fatalf("Recv() error = %v", err)
	}
	if string(got) != "ok" {
		t.Fatalf("Recv() = %q, want ok", got)
	}
	if _, err := reader.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("Recv() after DONE error = %v, want io.EOF", err)
	}
}

func TestSSEReader_MultiLineDataConcatenatedWithNewlines(t *testing.T) {
	// Per the SSE spec, multiple `data:` lines within a single event
	// are joined by '\n' before being delivered to the consumer.
	reader := NewSSEReader(io.NopCloser(strings.NewReader(
		"data: line1\n" +
			"data: line2\n" +
			"data: line3\n\n" +
			"data: [DONE]\n\n",
	)))

	got, err := reader.Recv()
	if err != nil {
		t.Fatalf("Recv() error = %v", err)
	}
	if string(got) != "line1\nline2\nline3" {
		t.Fatalf("Recv() = %q, want multi-line concatenation", got)
	}
}

func TestSSEReader_LargeSingleLinePayload(t *testing.T) {
	payload := strings.Repeat("x", 70*1024)
	reader := NewSSEReader(io.NopCloser(strings.NewReader("data: " + payload + "\n\ndata: [DONE]\n\n")))

	got, err := reader.Recv()
	if err != nil {
		t.Fatalf("Recv() error = %v", err)
	}
	if string(got) != payload {
		t.Fatalf("payload length = %d, want %d", len(got), len(payload))
	}
}

// TestSSEReader_OverOneMiBSingleLine guards the unified 10 MiB buffer
// cap. Anthropic's old in-adapter scanner used 1 MiB which would error
// out on large reasoning_delta payloads — the shared reader must not
// regress that limit downward.
func TestSSEReader_OverOneMiBSingleLine(t *testing.T) {
	payload := strings.Repeat("y", 2*1024*1024) // 2 MiB
	reader := NewSSEReader(io.NopCloser(strings.NewReader("data: " + payload + "\n\ndata: [DONE]\n\n")))

	got, err := reader.Recv()
	if err != nil {
		t.Fatalf("Recv() error = %v (1 MiB cap regressed?)", err)
	}
	if len(got) != len(payload) {
		t.Fatalf("payload length = %d, want %d", len(got), len(payload))
	}
}

func TestSSEReader_TrailerAfterDoneNotDelivered(t *testing.T) {
	reader := NewSSEReader(io.NopCloser(strings.NewReader(
		"data: one\n\n" +
			"data: [DONE]\n\n" +
			"data: trailer\n\n",
	)))

	got, err := reader.Recv()
	if err != nil {
		t.Fatalf("Recv() error = %v", err)
	}
	if string(got) != "one" {
		t.Fatalf("Recv() = %q, want one", got)
	}
	if _, err := reader.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("Recv() after DONE error = %v, want io.EOF", err)
	}
	if _, err := reader.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("Recv() after closed stream error = %v, want io.EOF", err)
	}
}

func TestSSEReader_NaturalEOFWithoutDoneIsLenient(t *testing.T) {
	// Anthropic-style: ends after the final event with no [DONE] sentinel.
	// The reader must surface the buffered events and then EOF cleanly,
	// not synthesize a "stream ended without [DONE]" error like the
	// previous strict implementation did.
	reader := NewSSEReader(io.NopCloser(strings.NewReader(
		"event: message_start\n" +
			"data: {\"type\":\"message_start\"}\n\n" +
			"event: message_stop\n" +
			"data: {\"type\":\"message_stop\"}\n\n",
	)))

	got, err := reader.Recv()
	if err != nil {
		t.Fatalf("first Recv() error = %v", err)
	}
	if string(got) != `{"type":"message_start"}` {
		t.Fatalf("first event = %q", got)
	}

	got, err = reader.Recv()
	if err != nil {
		t.Fatalf("second Recv() error = %v", err)
	}
	if string(got) != `{"type":"message_stop"}` {
		t.Fatalf("second event = %q", got)
	}

	if _, err := reader.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("third Recv() error = %v, want io.EOF (lenient natural EOF)", err)
	}
}

func TestSSEReader_TruncatedAfterDataNoBlankLineDelivers(t *testing.T) {
	// If upstream cuts after "data: one" but BEFORE the trailing blank
	// line, the buffered payload should still be delivered rather than
	// dropped on the floor.
	reader := NewSSEReader(io.NopCloser(strings.NewReader("data: one\n")))

	got, err := reader.Recv()
	if err != nil {
		t.Fatalf("Recv() error = %v", err)
	}
	if string(got) != "one" {
		t.Fatalf("Recv() = %q, want one", got)
	}
	if _, err := reader.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("post-truncation Recv() error = %v, want io.EOF", err)
	}
}

func TestSSEReader_ScannerErrorBubblesAsUpstreamError(t *testing.T) {
	// Hand-craft an io.Reader that errors mid-stream so we exercise the
	// scanner.Err() path (distinct from natural EOF).
	src := &errReader{
		data: []byte("data: one\n\ndata: two"),
		err:  errors.New("connection reset"),
	}
	reader := NewSSEReader(io.NopCloser(src))

	got, err := reader.Recv()
	if err != nil {
		t.Fatalf("first Recv() error = %v", err)
	}
	if string(got) != "one" {
		t.Fatalf("first Recv() = %q, want one", got)
	}

	// Second call: scanner now hits the underlying error.
	_, err = reader.Recv()
	var perr *llmtypes.Error
	if !errors.As(err, &perr) {
		t.Fatalf("err type = %T, want *llmtypes.Error", err)
	}
	if perr.ErrorKind != llmtypes.KindUpstream {
		t.Errorf("ErrorKind = %s, want %s", perr.ErrorKind, llmtypes.KindUpstream)
	}
}

// errReader returns its data buffer once, then yields a fixed error.
// Mirrors how a transport-level fault appears mid-stream.
type errReader struct {
	data   []byte
	err    error
	served bool
}

func (e *errReader) Read(p []byte) (int, error) {
	if !e.served {
		e.served = true
		return copy(p, e.data), nil
	}
	return 0, e.err
}
