package llmtypes

import (
	"context"
	"encoding/json"
)

// TranscriptionProvider is the transcription counterpart of Provider: an
// upstream that turns audio into text. Kept as its own interface (rather
// than widening Provider) so a chat-only vendor never has to stub a
// Transcribe method it cannot serve, and so routing can key the two
// provider kinds in separate maps.
type TranscriptionProvider interface {
	Name() string
	Transcribe(ctx context.Context, req *TranscriptionRequest) (*TranscriptionResponse, error)
	// TranscribeStream is the streaming counterpart of Transcribe: it opens
	// the upstream SSE and returns a TranscriptionStream the caller drains.
	// It mirrors Provider.CompleteStream so the transcription surface can
	// relay incremental text exactly like chat relays token deltas.
	TranscribeStream(ctx context.Context, req *TranscriptionRequest) (TranscriptionStream, error)
}

// TranscriptionStream is the transcription counterpart of Stream: an open
// upstream SSE the caller drains one event at a time. Recv/Close obey the
// same single-reader + prompt-Close contract as Stream; Summary returns
// best-effort totals for audit (only ChunkCount/FirstByteAt are meaningful
// for transcription — the assembled final text is accumulated by the caller
// from the relayed events, exactly as chat assembles its response).
type TranscriptionStream interface {
	Recv() (*TranscriptionEvent, error)
	Close() error
	Summary() *Summary
}

// TranscriptionEvent is one parsed upstream SSE event. The STT server emits
// `transcript.text.delta` (incremental) and `transcript.text.done` (full)
// events; Raw preserves the original `data:` payload so the gateway relays
// it to the client verbatim while Type/Delta/Text drive final-text assembly.
type TranscriptionEvent struct {
	Type  string `json:"type"`
	Delta string `json:"delta,omitempty"`
	Text  string `json:"text,omitempty"`

	// Raw is the original SSE data payload, kept so the relay forwards the
	// upstream frame byte-for-byte instead of a lossy re-encode.
	Raw []byte `json:"-"`
}

// TranscriptionRequest is the parsed form of an OpenAI
// `POST /v1/audio/transcriptions` multipart request. Unlike chat (a JSON
// body), the wire shape is multipart/form-data, so this struct carries the
// decoded fields rather than json tags — the handler fills it from the form
// and a provider re-encodes it for the upstream. Audio stays as raw bytes so
// it can be forwarded verbatim without a lossy round-trip.
type TranscriptionRequest struct {
	// Model is the alias (or concrete id) from the "model" form field. Routing
	// resolves it exactly like chat's Request.Model.
	Model string `json:"model"`
	// Audio is the raw file bytes from the "file" part; Filename is preserved
	// so the upstream (and audio decoders) can sniff the container format. The
	// json tags below exist only so the audit/result event serializes this
	// captured request cleanly (audio as base64) — multipart itself has no
	// json shape.
	Audio    []byte `json:"audio,omitempty"`
	Filename string `json:"filename,omitempty"`

	Language string `json:"language,omitempty"` // optional ISO-639-1 ("language")
	Prompt   string `json:"prompt,omitempty"`   // optional decoding hint ("prompt")
	// ResponseFormat mirrors OpenAI: json (default) | text | verbose_json |
	// srt | vtt. Empty means json.
	ResponseFormat string   `json:"response_format,omitempty"`
	Temperature    *float64 `json:"temperature,omitempty"` // optional ("temperature")
	// Stream is tri-state like chat Request.Stream: nil = omitted,
	// false = non-stream, true = SSE. Parsed from the "stream" form field.
	Stream *bool `json:"stream,omitempty"`
}

func (r *TranscriptionRequest) Validate() error {
	if r == nil {
		return &Error{Kind: KindBadRequest, Message: "request is nil"}
	}
	if r.Model == "" {
		return &Error{Kind: KindBadRequest, Message: "model is required"}
	}
	if len(r.Audio) == 0 {
		return &Error{Kind: KindBadRequest, Message: "file is required"}
	}
	return nil
}

// TranscriptionResponse is the OpenAI transcription result. The plain `json`
// and `text` formats only populate Text; `verbose_json` adds the timing
// fields. Extra preserves any upstream fields we don't model so a passthrough
// stays lossless.
type TranscriptionResponse struct {
	Text     string                 `json:"text"`
	Language string                 `json:"language,omitempty"`
	Duration float64                `json:"duration,omitempty"`
	Segments []TranscriptionSegment `json:"segments,omitempty"`
	Words    []TranscriptionWord    `json:"words,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// TranscriptionWord is a word-level timestamp (verbose_json with
// timestamp_granularities[]=word).
type TranscriptionWord struct {
	Word  string  `json:"word"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// TranscriptionSegment is a segment-level timestamp (verbose_json). Only the
// commonly consumed fields are modeled; the rest survive in Extra.
type TranscriptionSegment struct {
	ID    int     `json:"id"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`

	Extra map[string]json.RawMessage `json:"-"`
}
