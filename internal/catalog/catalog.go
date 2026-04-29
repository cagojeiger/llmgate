package catalog

import (
	_ "embed"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed opencode.yaml
var defaultCatalog []byte

type Catalog struct {
	Endpoints map[string]*Endpoint
	Models    map[string]*Model
	Defaults  Defaults
}

type Endpoint struct {
	Name         string
	Vendor       string
	BaseURL      string
	APIKey       string
	Protocol     string
	AuthScheme   string
	ExtraHeaders map[string]string
}

type Model struct {
	ID           string
	Endpoint     string
	Capabilities map[string]any
	Defaults     map[string]any
}

type Defaults struct {
	Model string
}

func LoadDefault() (*Catalog, error) {
	return loadBytes(defaultCatalog)
}

func LoadFile(path string) (*Catalog, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return loadBytes(b)
}

func Load() (*Catalog, error) {
	if path := os.Getenv("LLMGATE_CATALOG"); path != "" {
		return LoadFile(path)
	}
	return LoadDefault()
}

type rawCatalog struct {
	Vendor    string                 `yaml:"vendor"`
	BaseURL   string                 `yaml:"base_url"`
	AuthEnv   string                 `yaml:"auth_env"`
	Protocols map[string]rawProtocol `yaml:"protocols"`
	Defaults  Defaults               `yaml:"defaults"`
}

type rawProtocol struct {
	AuthScheme   string            `yaml:"auth_scheme"`
	ExtraHeaders map[string]string `yaml:"extra_headers"`
	Models       []rawModel        `yaml:"models"`
}

type rawModel struct {
	ID           string         `yaml:"id"`
	Capabilities map[string]any `yaml:"capabilities"`
	Defaults     map[string]any `yaml:"defaults"`
}

func loadBytes(b []byte) (*Catalog, error) {
	var raw rawCatalog
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return nil, err
	}

	apiKey := os.Getenv(raw.AuthEnv)
	if apiKey == "" {
		return nil, fmt.Errorf("catalog: env %s required for vendor %s is unset", raw.AuthEnv, raw.Vendor)
	}

	cat := &Catalog{
		Endpoints: make(map[string]*Endpoint),
		Models:    make(map[string]*Model),
		Defaults:  raw.Defaults,
	}

	for protocol, p := range raw.Protocols {
		endpointName := raw.Vendor + "-" + protocol
		cat.Endpoints[endpointName] = &Endpoint{
			Name:         endpointName,
			Vendor:       raw.Vendor,
			BaseURL:      raw.BaseURL,
			APIKey:       apiKey,
			Protocol:     protocol,
			AuthScheme:   p.AuthScheme,
			ExtraHeaders: copyStringMap(p.ExtraHeaders),
		}

		for _, m := range p.Models {
			key := strings.ToLower(m.ID)
			if _, exists := cat.Models[key]; exists {
				return nil, fmt.Errorf("catalog: duplicate model id %q", m.ID)
			}
			cat.Models[key] = &Model{
				ID:           m.ID,
				Endpoint:     endpointName,
				Capabilities: copyAnyMap(m.Capabilities),
				Defaults:     copyAnyMap(m.Defaults),
			}
		}
	}

	if len(cat.Models) == 0 {
		return nil, fmt.Errorf("catalog: no models declared")
	}
	if cat.Defaults.Model != "" {
		if _, ok := cat.Models[strings.ToLower(cat.Defaults.Model)]; !ok {
			return nil, fmt.Errorf("catalog: defaults.model %q references unknown model", cat.Defaults.Model)
		}
	}
	return cat, nil
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyAnyMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
