package audio

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"llmgate/internal/domain/llmtypes"
)

// defaultMaxAudioRequestBytes caps the multipart body. Audio uploads dwarf a
// chat JSON body (a minute of wav is megabytes), so this default is larger
// than chat's; the configured LLMGATE_MAX_REQUEST_BYTES still overrides.
const defaultMaxAudioRequestBytes = 25 << 20

// multipartMemory is how much of the parsed form is buffered in memory before
// spilling parts to temp files. The body is already capped by MaxBytesReader.
const multipartMemory = 8 << 20

func decodeTranscriptionRequest(w http.ResponseWriter, r *http.Request, maxBytes int64) (*llmtypes.TranscriptionRequest, int64, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	if err := r.ParseMultipartForm(multipartMemory); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, 0, &llmtypes.Error{
				Kind:    llmtypes.KindBadRequest,
				Message: fmt.Sprintf("request body exceeds %d bytes (LLMGATE_MAX_REQUEST_BYTES)", tooLarge.Limit),
			}
		}
		return nil, 0, &llmtypes.Error{Kind: llmtypes.KindBadRequest, Message: "parse multipart form: " + err.Error()}
	}
	// Audio parts larger than multipartMemory spill to temp files that net/http
	// does not auto-remove; the audio is copied into req.Audio below, so drop the
	// temp files when this returns.
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	req := &llmtypes.TranscriptionRequest{
		Model:          r.FormValue("model"),
		Language:       r.FormValue("language"),
		Prompt:         r.FormValue("prompt"),
		ResponseFormat: r.FormValue("response_format"),
	}

	// A missing file part is left as empty audio so Validate produces the
	// canonical "file is required" bad request, keeping error shaping in one
	// place. Any other FormFile error is a malformed body.
	file, header, err := r.FormFile("file")
	if err != nil {
		if !errors.Is(err, http.ErrMissingFile) {
			return nil, 0, &llmtypes.Error{Kind: llmtypes.KindBadRequest, Message: "read file part: " + err.Error()}
		}
	} else {
		defer file.Close()
		audio, rerr := io.ReadAll(file)
		if rerr != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(rerr, &tooLarge) {
				return nil, 0, &llmtypes.Error{
					Kind:    llmtypes.KindBadRequest,
					Message: fmt.Sprintf("request body exceeds %d bytes (LLMGATE_MAX_REQUEST_BYTES)", tooLarge.Limit),
				}
			}
			return nil, 0, &llmtypes.Error{Kind: llmtypes.KindBadRequest, Message: "read file part: " + rerr.Error()}
		}
		req.Audio = audio
		req.Filename = header.Filename
	}

	if t := r.FormValue("temperature"); t != "" {
		f, perr := strconv.ParseFloat(t, 64)
		if perr != nil {
			return nil, 0, &llmtypes.Error{Kind: llmtypes.KindBadRequest, Message: "temperature must be a number: " + perr.Error()}
		}
		req.Temperature = &f
	}

	// Stream is tri-state: absent leaves it nil (non-stream), matching chat.
	if s := r.FormValue("stream"); s != "" {
		b, perr := strconv.ParseBool(s)
		if perr != nil {
			return nil, 0, &llmtypes.Error{Kind: llmtypes.KindBadRequest, Message: "stream must be a boolean: " + perr.Error()}
		}
		req.Stream = &b
	}

	// RequestBytes accounts the audio payload — the meaningful measure of a
	// transcription request's size, and what the audit record surfaces.
	return req, int64(len(req.Audio)), nil
}

func modelAllowed(model string, allowed []string) bool {
	if model == "" || len(allowed) == 0 {
		return true
	}
	for _, alias := range allowed {
		if strings.EqualFold(model, alias) {
			return true
		}
	}
	return false
}

func modelNotAllowedError() *llmtypes.Error {
	return &llmtypes.Error{Kind: llmtypes.KindForbidden, Message: "model not allowed"}
}
