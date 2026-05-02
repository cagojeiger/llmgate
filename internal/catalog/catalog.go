// Package catalog loads the gateway's per-model endpoint table from a
// directory of yaml files.
//
// Layout (under the root passed to LoadDir):
//
//	models/<id>.yaml          one yaml per endpoint (id + vendor + type +
//	                          base_url + auth_env). file = endpoint.
//	aliases/<name>.yaml       one yaml per alias (alias + chain).
//
// Each models/*.yaml registers exactly one Endpoint and one Model with
// the same id, so two yaml files for the same vendor model name but
// different auth_env coexist as separate endpoints — useful when one
// person runs several subscriptions, but the gateway itself needs no
// special code for that case.
//
// Routing policy (fallback eligibility, circuit breaker) is not part
// of the catalog. It lives in env-driven config and reaches the router
// through main.go — the catalog's job is data only.
//
// The default catalog ships at the repo root under catalog/. At runtime
// LLMGATE_CATALOG (a directory path) overrides; otherwise Load reads
// from cwd's ./catalog.
package catalog

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

const defaultDir = "./catalog"

type Catalog struct {
	Endpoints map[string]*Endpoint
	Models    map[string]*Model
	Aliases   map[string]*Alias
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

// rawAlias is the shape of aliases/<name>.yaml.
type rawAlias struct {
	Alias string   `yaml:"alias"`
	Chain []string `yaml:"chain"`
}

// LoadDir reads a catalog from the given directory on disk. The directory
// must contain a models/ subdirectory (at minimum); aliases/ is optional.
func LoadDir(dir string) (*Catalog, error) {
	return loadFS(os.DirFS(dir))
}

// Load returns the catalog at LLMGATE_CATALOG (a directory path) or
// from cwd's ./catalog when the env is unset.
func Load() (*Catalog, error) {
	dir := os.Getenv("LLMGATE_CATALOG")
	if dir == "" {
		dir = defaultDir
	}
	return LoadDir(dir)
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

	if err := walkYAMLOptional(fsys, "aliases", func(name string, data []byte) error {
		var a rawAlias
		if err := yaml.Unmarshal(data, &a); err != nil {
			return fmt.Errorf("aliases/%s: %w", name, err)
		}
		return registerAlias(cat, a)
	}); err != nil {
		return nil, err
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

// walkYAMLOptional is walkYAML but treats a missing dir as empty (so
// aliases/ being absent means "no aliases", not an error).
func walkYAMLOptional(fsys fs.FS, dir string, fn func(name string, data []byte) error) error {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("catalog: read %s: %w", dir, err)
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
