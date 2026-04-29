package provider

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestSSEReaderNormal(t *testing.T) {
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

func TestSSEReaderIgnoresEmptyLinesAndComments(t *testing.T) {
	reader := NewSSEReader(io.NopCloser(strings.NewReader(
		"\n" +
			": comment\n\n" +
			"event: message\n" +
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
}

func TestSSEReaderLargePayload(t *testing.T) {
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

func TestSSEReaderTrailerAfterDoneNotReturned(t *testing.T) {
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

func TestSSEReaderTruncatedStream(t *testing.T) {
	reader := NewSSEReader(io.NopCloser(strings.NewReader("data: one\n\n")))

	got, err := reader.Recv()
	if err != nil {
		t.Fatalf("Recv() error = %v", err)
	}
	if string(got) != "one" {
		t.Fatalf("Recv() = %q, want one", got)
	}

	_, err = reader.Recv()
	var perr *Error
	if !errors.As(err, &perr) {
		t.Fatalf("Recv() error type = %T, want *Error", err)
	}
	if perr.Kind != KindUpstream {
		t.Fatalf("Kind = %s, want %s", perr.Kind, KindUpstream)
	}
}
