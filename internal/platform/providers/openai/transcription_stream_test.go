package openai

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"llmgate/internal/domain/llmtypes"
)

func TestTranscribeStream_RelaysDeltaAndDone(t *testing.T) {
	var gotStream, gotAccept string
	srv := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/transcriptions" {
			t.Errorf("path = %q, want /audio/transcriptions", r.URL.Path)
		}
		gotAccept = r.Header.Get("Accept")
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Fatalf("parse content-type: %v", err)
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, perr := mr.NextPart()
			if perr == io.EOF {
				break
			}
			if perr != nil {
				t.Fatalf("read part: %v", perr)
			}
			if part.FormName() == "stream" {
				b, _ := io.ReadAll(part)
				gotStream = string(b)
			}
		}

		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEChunk(t, w, `{"type":"transcript.text.delta","delta":"he"}`)
		writeSSEChunk(t, w, `{"type":"transcript.text.delta","delta":"llo"}`)
		writeSSEChunk(t, w, `{"type":"transcript.text.done","text":"hello"}`)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := mustNewTranscription(t, TranscriptionConfig{BaseURL: srv.URL, APIKey: "k", Name: "qwen", HTTPClient: srv.Client})
	stream, err := c.TranscribeStream(context.Background(), &llmtypes.TranscriptionRequest{
		Model:    "qwen-asr",
		Audio:    []byte("RIFFdata"),
		Filename: "sample.wav",
	})
	if err != nil {
		t.Fatalf("TranscribeStream() error = %v", err)
	}
	defer stream.Close()

	var deltas strings.Builder
	var doneText string
	events := 0
	for {
		event, rerr := stream.Recv()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			t.Fatalf("Recv() error = %v", rerr)
		}
		events++
		switch event.Type {
		case "transcript.text.delta":
			deltas.WriteString(event.Delta)
		case "transcript.text.done":
			doneText = event.Text
		}
		if len(event.Raw) == 0 {
			t.Errorf("event.Raw empty, want verbatim payload for %q", event.Type)
		}
	}

	if gotAccept != "text/event-stream" {
		t.Errorf("Accept = %q, want text/event-stream", gotAccept)
	}
	if gotStream != "true" {
		t.Errorf("stream field = %q, want true", gotStream)
	}
	if events != 3 {
		t.Errorf("events = %d, want 3", events)
	}
	if deltas.String() != "hello" {
		t.Errorf("concatenated deltas = %q, want hello", deltas.String())
	}
	if doneText != "hello" {
		t.Errorf("done text = %q, want hello", doneText)
	}

	sum := stream.Summary()
	if sum == nil || sum.ChunkCount != 3 {
		t.Errorf("Summary ChunkCount = %+v, want 3", sum)
	}
	if sum.FirstByteAt.IsZero() {
		t.Error("Summary.FirstByteAt is zero, want set after first event")
	}
}

func TestTranscribeStream_UpstreamStatusErrorClassified(t *testing.T) {
	srv := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down","type":"rate_limit_error"}}`))
	}))
	defer srv.Close()

	c := mustNewTranscription(t, TranscriptionConfig{BaseURL: srv.URL, Name: "qwen", HTTPClient: srv.Client})
	_, err := c.TranscribeStream(context.Background(), &llmtypes.TranscriptionRequest{Model: "qwen-asr", Audio: []byte("d")})
	perr := requireProviderError(t, err)
	if perr.Kind != llmtypes.KindRateLimit || perr.Provider != "qwen" {
		t.Errorf("error kind/provider = %q/%q, want rate_limit/qwen", perr.Kind, perr.Provider)
	}
}

func TestTranscribeStream_MidStreamErrorSurfaced(t *testing.T) {
	srv := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEChunk(t, w, `{"type":"transcript.text.delta","delta":"a"}`)
		writeSSEChunk(t, w, `{"error":{"message":"stream exploded","type":"upstream_error"}}`)
	}))
	defer srv.Close()

	c := mustNewTranscription(t, TranscriptionConfig{BaseURL: srv.URL, Name: "qwen", HTTPClient: srv.Client})
	stream, err := c.TranscribeStream(context.Background(), &llmtypes.TranscriptionRequest{Model: "qwen-asr", Audio: []byte("d")})
	if err != nil {
		t.Fatalf("TranscribeStream() error = %v", err)
	}
	defer stream.Close()

	if _, rerr := stream.Recv(); rerr != nil {
		t.Fatalf("first Recv() error = %v", rerr)
	}
	_, rerr := stream.Recv()
	perr := requireProviderError(t, rerr)
	if perr.Kind != llmtypes.KindUpstream {
		t.Errorf("Kind = %q, want upstream", perr.Kind)
	}
	if !strings.Contains(string(perr.Raw), "stream exploded") {
		t.Errorf("Raw = %q, want original error preserved", string(perr.Raw))
	}
}
