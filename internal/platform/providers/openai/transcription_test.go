package openai

import (
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"llmgate/internal/domain/llmtypes"
)

func mustNewTranscription(t *testing.T, cfg TranscriptionConfig) *TranscriptionClient {
	t.Helper()
	c, err := NewTranscription(cfg)
	if err != nil {
		t.Fatalf("NewTranscription() error = %v", err)
	}
	return c
}

func TestTranscriptionClient_JSONResponse(t *testing.T) {
	var gotModel, gotLanguage, gotFilename, gotAuth string
	srv := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/transcriptions" {
			t.Errorf("path = %q, want /audio/transcriptions", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "multipart/form-data" {
			t.Fatalf("content-type = %q (err %v), want multipart/form-data", r.Header.Get("Content-Type"), err)
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
			switch part.FormName() {
			case "file":
				gotFilename = part.FileName()
			case "model":
				b, _ := io.ReadAll(part)
				gotModel = string(b)
			case "language":
				b, _ := io.ReadAll(part)
				gotLanguage = string(b)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"hello world","language":"en","duration":1.5}`))
	}))
	defer srv.Close()

	c := mustNewTranscription(t, TranscriptionConfig{BaseURL: srv.URL, APIKey: "k", Name: "qwen", HTTPClient: srv.Client})

	resp, err := c.Transcribe(context.Background(), &llmtypes.TranscriptionRequest{
		Model:    "qwen-asr",
		Audio:    []byte("RIFFdata"),
		Filename: "sample.wav",
		Language: "en",
	})
	if err != nil {
		t.Fatalf("Transcribe() error = %v", err)
	}
	if resp.Text != "hello world" || resp.Language != "en" || resp.Duration != 1.5 {
		t.Errorf("resp = %+v, want text/lang/duration populated", resp)
	}
	if gotModel != "qwen-asr" || gotLanguage != "en" || gotFilename != "sample.wav" {
		t.Errorf("multipart fields = model:%q language:%q filename:%q", gotModel, gotLanguage, gotFilename)
	}
	if gotAuth != "Bearer k" {
		t.Errorf("Authorization = %q, want Bearer k", gotAuth)
	}
}

func TestTranscriptionClient_TextResponseFormat(t *testing.T) {
	srv := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("just plain text"))
	}))
	defer srv.Close()

	c := mustNewTranscription(t, TranscriptionConfig{BaseURL: srv.URL, Name: "qwen", HTTPClient: srv.Client})
	resp, err := c.Transcribe(context.Background(), &llmtypes.TranscriptionRequest{
		Model:          "qwen-asr",
		Audio:          []byte("data"),
		ResponseFormat: "text",
	})
	if err != nil {
		t.Fatalf("Transcribe() error = %v", err)
	}
	if resp.Text != "just plain text" {
		t.Errorf("Text = %q, want plain text body", resp.Text)
	}
}

func TestTranscriptionClient_NoAuthHeaderWhenUnauthenticated(t *testing.T) {
	var hadAuth bool
	srv := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadAuth = r.Header["Authorization"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"ok"}`))
	}))
	defer srv.Close()

	c := mustNewTranscription(t, TranscriptionConfig{BaseURL: srv.URL, Name: "qwen", HTTPClient: srv.Client})
	if _, err := c.Transcribe(context.Background(), &llmtypes.TranscriptionRequest{Model: "qwen-asr", Audio: []byte("d")}); err != nil {
		t.Fatalf("Transcribe() error = %v", err)
	}
	if hadAuth {
		t.Error("Authorization header present, want none for unauthenticated upstream")
	}
}

func TestTranscriptionClient_UpstreamErrorClassified(t *testing.T) {
	srv := newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down","type":"rate_limit_error"}}`))
	}))
	defer srv.Close()

	c := mustNewTranscription(t, TranscriptionConfig{BaseURL: srv.URL, Name: "qwen", HTTPClient: srv.Client})
	_, err := c.Transcribe(context.Background(), &llmtypes.TranscriptionRequest{Model: "qwen-asr", Audio: []byte("d")})
	perr := requireProviderError(t, err)
	if perr.Kind != llmtypes.KindRateLimit || perr.Provider != "qwen" {
		t.Errorf("error kind/provider = %q/%q, want rate_limit/qwen", perr.Kind, perr.Provider)
	}
}

func TestTranscriptionClient_ValidationRejectsMissingFields(t *testing.T) {
	c := mustNewTranscription(t, TranscriptionConfig{BaseURL: "http://x", Name: "qwen"})
	_, err := c.Transcribe(context.Background(), &llmtypes.TranscriptionRequest{Model: "qwen-asr"})
	perr := requireProviderError(t, err)
	if perr.Kind != llmtypes.KindBadRequest {
		t.Errorf("kind = %q, want bad_request for missing audio", perr.Kind)
	}
	if !strings.Contains(perr.Error(), "file is required") {
		t.Errorf("message = %q, want file-required", perr.Error())
	}
}
