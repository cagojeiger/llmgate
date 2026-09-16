package llmtypes

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
)

// EmbeddingProvider is separate from chat because vector inference has no stream.
type EmbeddingProvider interface {
	Name() string
	Embed(context.Context, *EmbeddingRequest) (*EmbeddingResponse, error)
}

// EmbeddingRequest initially supports text inputs, not pre-tokenized IDs.
type EmbeddingRequest struct {
	Model          string          `json:"model"`
	Input          json.RawMessage `json:"input"`
	EncodingFormat string          `json:"encoding_format,omitempty"`
	Dimensions     *int            `json:"dimensions,omitempty"`
	User           string          `json:"user,omitempty"`
}

func (r *EmbeddingRequest) Inputs() ([]string, error) {
	var texts []string
	if r == nil {
		return nil, &Error{Kind: KindBadRequest, Message: "request is nil"}
	}
	input := bytes.TrimSpace(r.Input)
	if len(input) > 0 && input[0] == '"' {
		var text string
		if err := json.Unmarshal(input, &text); err == nil {
			texts = []string{text}
		}
	} else if len(input) > 0 && input[0] == '[' {
		if err := json.Unmarshal(input, &texts); err != nil {
			texts = nil
		}
	}
	if len(texts) == 0 || len(texts) > 128 {
		return nil, &Error{Kind: KindBadRequest, Message: "input must be a string or an array of 1..128 strings"}
	}
	for _, text := range texts {
		if strings.TrimSpace(text) == "" {
			return nil, &Error{Kind: KindBadRequest, Message: "input strings must not be empty"}
		}
	}
	return texts, nil
}

func (r *EmbeddingRequest) Validate() error {
	if _, err := r.Inputs(); err != nil {
		return err
	}
	if strings.TrimSpace(r.Model) == "" {
		return &Error{Kind: KindBadRequest, Message: "model is required"}
	}
	if r.EncodingFormat != "" && r.EncodingFormat != "float" && r.EncodingFormat != "base64" {
		return &Error{Kind: KindBadRequest, Message: "encoding_format must be float or base64"}
	}
	if r.Dimensions != nil && (*r.Dimensions < 1 || *r.Dimensions > 65536) {
		return &Error{Kind: KindBadRequest, Message: "dimensions must be 1..65536"}
	}
	return nil
}

type EmbeddingResponse struct {
	Object string      `json:"object"`
	Data   []Embedding `json:"data"`
	Model  string      `json:"model"`
	Usage  *Usage      `json:"usage,omitempty"`
}

type Embedding struct {
	Object    string          `json:"object"`
	Index     int             `json:"index"`
	Embedding json.RawMessage `json:"embedding"`
}
