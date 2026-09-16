package gateway

import (
	"fmt"

	"llmgate/internal/domain/catalog"
	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/platform/relaytoken"
)

// Fixed worker profiles become runtime catalog data, never writes to operator YAML.
func workerCatalog(issuer *relaytoken.Issuer) (*catalog.Catalog, error) {
	if issuer == nil {
		return nil, nil
	}
	out := &catalog.Catalog{Models: map[string]*catalog.Model{}, Aliases: map[string]*catalog.Alias{}}
	for name, p := range issuer.Profiles() {
		var id string
		var api catalog.API
		switch name {
		case "embedding":
			id, api = "qwen3-embedding-0.6b", catalog.APIEmbeddings
		case "stt":
			id, api = "qwen3-asr-0.6b", catalog.APITranscription
		default:
			return nil, fmt.Errorf("unsupported automatic worker profile %q", name)
		}
		out.Models[id] = &catalog.Model{ID: id, Vendor: "local-mlx", Protocol: llmtypes.ProtocolOpenAI, API: api, BaseURL: "http://" + p.CallerAddress + "/v1", NewConnectionPerRequest: true}
		out.Aliases[name] = &catalog.Alias{Alias: name, Chain: []string{id}}
	}
	return out, nil
}
