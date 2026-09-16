package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"llmgate/internal/domain/llmtypes"
)

func TestEmbeddingRejectsMalformedVectors(t *testing.T) {
	for _, vector := range []string{`[]`, `[null]`, `[1,2]`, `"bad"`} {
		t.Run(vector, func(t *testing.T) {
			dim := 1
			out := &llmtypes.EmbeddingResponse{Object: "list", Data: []llmtypes.Embedding{{Object: "embedding", Index: 0, Embedding: json.RawMessage(vector)}}}
			if validateEmbeddings(out, &llmtypes.EmbeddingRequest{Dimensions: &dim}, 1) == nil {
				t.Fatal("accepted malformed vector")
			}
		})
	}
	out := &llmtypes.EmbeddingResponse{Object: "list", Data: []llmtypes.Embedding{{Object: "embedding", Index: 0, Embedding: json.RawMessage(`[1]`)}, {Object: "embedding", Index: 0, Embedding: json.RawMessage(`[1]`)}}}
	if validateEmbeddings(out, &llmtypes.EmbeddingRequest{}, 2) == nil {
		t.Fatal("accepted duplicate index")
	}
	if n, err := embeddingSize(json.RawMessage(`"AACAPw=="`), "base64"); err != nil || n != 1 {
		t.Fatalf("base64: %d %v", n, err)
	}
}

func TestEmbeddingPropagatesUpstreamFailureWithoutRetry(t *testing.T) {
	count := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { count++; w.WriteHeader(503) }))
	defer srv.Close()
	p, err := NewEmbedding(EmbeddingConfig{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Embed(context.Background(), &llmtypes.EmbeddingRequest{Model: "qwen", Input: json.RawMessage(`"hello"`)})
	if err == nil || count != 1 {
		t.Fatalf("error=%v calls=%d", err, count)
	}
}
