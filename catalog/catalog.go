// Package catalog loads the gateway's per-model endpoint table and
// fallback policy from a directory of yaml files.
//
// Layout:
//
//	<root>/
//	  models/                one yaml per endpoint (id + vendor + type +
//	                         base_url + auth_env). file = endpoint.
//	  fallback/
//	    <alias>.yaml         one yaml per alias (alias + chain). file
//	                         name is informational; the alias name in
//	                         the yaml is canonical.
//	    policy.yaml          single global policy (on_kinds + circuit +
//	                         defaults).
//
// Each models/*.yaml registers exactly one Endpoint and one Model with
// the same id, so two yaml files for the same vendor model name but
// different auth_env coexist as separate endpoints — useful when one
// person runs several subscriptions, but the gateway itself needs no
// special code for that case.
package catalog

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed all:default
var defaultFS embed.FS

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

// rawModel is the on-disk shape of a models/*.yaml file. Each file
// declares one endpoint + one model.
type rawModel struct {
	ID         string `yaml:"id"`
	Vendor     string `yaml:"vendor"`
	Type       string `yaml:"type"`
	BaseURL    string `yaml:"base_url"`
	AuthEnv    string `yaml:"auth_env"`
	AuthScheme string `yaml:"auth_scheme"`
}

// rawAlias is the shape of fallback/<name>.yaml (excluding policy.yaml).
type rawAlias struct {
	Alias string   `yaml:"alias"`
	Chain []string `yaml:"chain"`
}

// rawPolicy is the shape of fallback/policy.yaml — global routing policy
// plus defaults that don't belong to any single alias.
type rawPolicy struct {
	OnKinds  []string   `yaml:"on_kinds"`
	Circuit  rawCircuit `yaml:"circuit"`
	Defaults Defaults   `yaml:"defaults"`
}

type rawCircuit struct {
	FailuresToOpen int           `yaml:"failures_to_open"`
	OpenDuration   time.Duration `yaml:"open_duration"`
}

// LoadDir reads a catalog from the given directory on disk. The directory
// must contain a models/ subdirectory (at minimum); fallback/ is optional.
func LoadDir(dir string) (*Catalog, error) {
	return loadFS(os.DirFS(dir))
}

// LoadDefault reads the catalog embedded at build time under default/.
// Used when LLMGATE_CATALOG is unset.
func LoadDefault() (*Catalog, error) {
	sub, err := fs.Sub(defaultFS, "default")
	if err != nil {
		return nil, fmt.Errorf("catalog: open embedded default: %w", err)
	}
	return loadFS(sub)
}

// Load returns the catalog pointed to by LLMGATE_CATALOG (a directory
// path) or the embedded default if the env is unset.
func Load() (*Catalog, error) {
	if dir := os.Getenv("LLMGATE_CATALOG"); dir != "" {
		return LoadDir(dir)
	}
	return LoadDefault()
}

func loadFS(fsys fs.FS) (*Catalog, error) {
	cat := &Catalog{
		Endpoints: make(map[string]*Endpoint),
		Models:    make(map[string]*Model),
		Aliases:   make(map[string]*Alias),
	}

	if err := walkYAML(fsys, "models", func(name string, data []byte) error {
		var m rawModel
		if err := yaml.Unmarshal(data, &m); err != nil {
			return fmt.Errorf("models/%s: %w", name, err)
		}
		return registerModel(cat, m)
	}); err != nil {
		return nil, err
	}
	if len(cat.Models) == 0 {
		return nil, fmt.Errorf("catalog: no models loaded from models/")
	}

	var policy rawPolicy
	havePolicy := false
	if err := walkYAMLOptional(fsys, "fallback", func(name string, data []byte) error {
		if name == "policy.yaml" || name == "policy.yml" {
			if err := yaml.Unmarshal(data, &policy); err != nil {
				return fmt.Errorf("fallback/%s: %w", name, err)
			}
			havePolicy = true
			return nil
		}
		var a rawAlias
		if err := yaml.Unmarshal(data, &a); err != nil {
			return fmt.Errorf("fallback/%s: %w", name, err)
		}
		return registerAlias(cat, a)
	}); err != nil {
		return nil, err
	}

	if havePolicy {
		cat.Fallback = FallbackPolicy{
			OnKinds:         append([]string(nil), policy.OnKinds...),
			CircuitFailures: policy.Circuit.FailuresToOpen,
			CircuitOpen:     policy.Circuit.OpenDuration,
		}
		cat.Defaults = policy.Defaults
	}

	if cat.Defaults.Model != "" {
		if _, ok := cat.Models[strings.ToLower(cat.Defaults.Model)]; !ok {
			return nil, fmt.Errorf("catalog: defaults.model %q references unknown model", cat.Defaults.Model)
		}
	}
	for _, a := range cat.Aliases {
		for _, m := range a.Chain {
			if _, ok := cat.Models[strings.ToLower(m)]; !ok {
				return nil, fmt.Errorf("catalog: alias %q references unknown model %q", a.Name, m)
			}
		}
	}

	return cat, nil
}

// registerModel turns one models/*.yaml into one Endpoint + one Model.
// They share the id so router lookups stay 1:1; this also makes "same
// vendor model, different auth_env" trivially work as two distinct
// endpoints.
func registerModel(cat *Catalog, m rawModel) error {
	if m.ID == "" {
		return fmt.Errorf("model: id is required")
	}
	if m.Vendor == "" || m.Type == "" || m.BaseURL == "" || m.AuthEnv == "" {
		return fmt.Errorf("model %q: vendor / type / base_url / auth_env are all required", m.ID)
	}
	apiKey := os.Getenv(m.AuthEnv)
	if apiKey == "" {
		return fmt.Errorf("model %q: env %s is unset", m.ID, m.AuthEnv)
	}

	key := strings.ToLower(m.ID)
	if _, exists := cat.Models[key]; exists {
		return fmt.Errorf("catalog: duplicate model id %q", m.ID)
	}
	cat.Endpoints[key] = &Endpoint{
		Name:       m.ID,
		Vendor:     m.Vendor,
		BaseURL:    m.BaseURL,
		APIKey:     apiKey,
		Protocol:   m.Type,
		AuthScheme: m.AuthScheme,
	}
	cat.Models[key] = &Model{
		ID:       m.ID,
		Endpoint: m.ID,
	}
	return nil
}

func registerAlias(cat *Catalog, a rawAlias) error {
	if a.Alias == "" {
		return fmt.Errorf("alias: name (alias field) is required")
	}
	if len(a.Chain) == 0 {
		return fmt.Errorf("alias %q: empty chain", a.Alias)
	}
	key := strings.ToLower(a.Alias)
	if _, exists := cat.Models[key]; exists {
		return fmt.Errorf("catalog: alias %q collides with model id of the same name", a.Alias)
	}
	if _, exists := cat.Aliases[key]; exists {
		return fmt.Errorf("catalog: duplicate alias %q", a.Alias)
	}
	cat.Aliases[key] = &Alias{
		Name:  a.Alias,
		Chain: append([]string(nil), a.Chain...),
	}
	return nil
}

// walkYAML iterates *.yaml / *.yml files in dir. Returns an error if the
// directory is missing — used for required dirs like models/.
func walkYAML(fsys fs.FS, dir string, fn func(name string, data []byte) error) error {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return fmt.Errorf("catalog: read %s: %w", dir, err)
	}
	return walkEntries(fsys, dir, entries, fn)
}

// walkYAMLOptional is walkYAML but treats a missing dir as empty (no
// aliases / no policy is OK).
func walkYAMLOptional(fsys fs.FS, dir string, fn func(name string, data []byte) error) error {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil
	}
	return walkEntries(fsys, dir, entries, fn)
}

func walkEntries(fsys fs.FS, dir string, entries []fs.DirEntry, fn func(name string, data []byte) error) error {
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		data, err := fs.ReadFile(fsys, path.Join(dir, name))
		if err != nil {
			return fmt.Errorf("catalog: read %s/%s: %w", dir, name, err)
		}
		if err := fn(name, data); err != nil {
			return err
		}
	}
	return nil
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
