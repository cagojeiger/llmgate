package gateway

import (
	"errors"
	"fmt"
	"strings"

	"llmgate/internal/domain/catalog"
	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/routing"
	"llmgate/internal/platform/providers/openai"
)

func buildEmbeddingModels(cat *catalog.Catalog) (routing.EmbeddingModels, error) {
	models := make(routing.EmbeddingModels)
	for id, m := range cat.Models {
		if m.API != catalog.APIEmbeddings {
			continue
		}
		if m.Protocol != llmtypes.ProtocolOpenAI {
			return nil, fmt.Errorf("embedding model %q requires openai protocol", id)
		}
		key, err := readAuthKey(m)
		if err != nil {
			var missing *missingAuthKeyError
			if !errors.As(err, &missing) || m.AuthEnv != "" || m.AuthScheme != "" {
				return nil, err
			}
		}
		p, err := openai.NewEmbedding(openai.EmbeddingConfig{BaseURL: m.BaseURL, APIKey: key, AuthScheme: m.AuthScheme, Name: m.Vendor})
		if err != nil {
			return nil, fmt.Errorf("embedding model %q: %w", id, err)
		}
		models[id] = p
	}
	for name, alias := range cat.Aliases {
		for _, id := range alias.Chain {
			if model, ok := cat.Models[strings.ToLower(id)]; ok && model.API == catalog.APIEmbeddings && len(alias.Chain) != 1 {
				return nil, fmt.Errorf("embedding alias %q must have exactly one model to preserve its vector space", name)
			}
		}
	}
	return models, nil
}
