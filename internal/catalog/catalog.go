package catalog

import (
	_ "embed"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed opencode.yaml
var defaultCatalog []byte

type Catalog struct {
	Endpoints map[string]*Endpoint
	Models    map[string]*Model
	Aliases   map[string]*Alias
	Fallback  FallbackPolicy
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

// Alias maps a logical name (e.g. "coder") to an ordered list of concrete
// model IDs to try in priority order. Routing tries chain[0] first; on a
// fallback-eligible failure it tries chain[1]; and so on.
type Alias struct {
	Name  string
	Chain []string
}

// FallbackPolicy declares when the router treats a failure as eligible for
// the next chain entry, and how circuit breaking suppresses dead models.
//
// OnKinds is a string list (matched against provider.Kind values) so this
// package stays free of upstream-error semantics. Empty list = no errors
// are fallback-eligible (i.e. fallback is effectively disabled).
type FallbackPolicy struct {
	OnKinds         []string
	CircuitFailures int
	CircuitOpen     time.Duration
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
	Aliases   map[string]rawAlias    `yaml:"aliases"`
	Fallback  rawFallback            `yaml:"fallback"`
	Defaults  Defaults               `yaml:"defaults"`
}

type rawAlias struct {
	Chain []string `yaml:"chain"`
}

type rawFallback struct {
	OnKinds []string    `yaml:"on_kinds"`
	Circuit rawCircuit  `yaml:"circuit"`
}

type rawCircuit struct {
	FailuresToOpen int           `yaml:"failures_to_open"`
	OpenDuration   time.Duration `yaml:"open_duration"`
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

	if len(raw.Aliases) > 0 {
		cat.Aliases = make(map[string]*Alias, len(raw.Aliases))
		for name, ra := range raw.Aliases {
			key := strings.ToLower(name)
			if _, exists := cat.Models[key]; exists {
				return nil, fmt.Errorf("catalog: alias %q collides with model id of the same name", name)
			}
			if _, exists := cat.Aliases[key]; exists {
				return nil, fmt.Errorf("catalog: duplicate alias %q", name)
			}
			if len(ra.Chain) == 0 {
				return nil, fmt.Errorf("catalog: alias %q has empty chain", name)
			}
			for _, m := range ra.Chain {
				if _, ok := cat.Models[strings.ToLower(m)]; !ok {
					return nil, fmt.Errorf("catalog: alias %q references unknown model %q", name, m)
				}
			}
			cat.Aliases[key] = &Alias{Name: name, Chain: append([]string(nil), ra.Chain...)}
		}
	}

	cat.Fallback = FallbackPolicy{
		OnKinds:         append([]string(nil), raw.Fallback.OnKinds...),
		CircuitFailures: raw.Fallback.Circuit.FailuresToOpen,
		CircuitOpen:     raw.Fallback.Circuit.OpenDuration,
	}

	return cat, nil
}

// ResolveAlias returns the ordered chain for the given name. If name is an
// alias, returns its declared chain. Otherwise returns a single-element
// slice containing the name itself (so callers can treat both cases the
// same way). Lookup is case-insensitive.
func (c *Catalog) ResolveAlias(name string) []string {
	if c == nil {
		return []string{name}
	}
	if alias, ok := c.Aliases[strings.ToLower(name)]; ok {
		return append([]string(nil), alias.Chain...)
	}
	return []string{name}
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
