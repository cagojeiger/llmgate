package gateway

import (
	"llmgate/internal/domain/catalog"
	"testing"
)

func TestEmbeddingAliasCannotMixVectorSpaces(t *testing.T) {
	cat := &catalog.Catalog{Models: map[string]*catalog.Model{"qwen": {ID: "qwen", Vendor: "local", Protocol: "openai", API: catalog.APIEmbeddings, BaseURL: "http://127.0.0.1:18080/v1"}}, Aliases: map[string]*catalog.Alias{"embed": {Chain: []string{"QWEN", "other"}}}}
	if _, err := buildEmbeddingModels(cat); err == nil {
		t.Fatal("accepted cross-model fallback")
	}
	cat.Aliases["embed"].Chain = []string{"QWEN"}
	if models, err := buildEmbeddingModels(cat); err != nil || len(models) != 1 {
		t.Fatalf("models=%v error=%v", models, err)
	}
}
