// Package catalog loads the gateway's per-model registration table from a
// directory of yaml files.
//
// Layout (under the root passed to LoadDir):
//
//	models/<id>.yaml          one yaml per model — id + vendor + protocol +
//	                          base_url + auth_env + auth_scheme. file = model.
//	aliases/<name>.yaml       one yaml per alias — alias + chain.
//
// Each models/*.yaml registers one Model. Two yaml files for the same vendor
// model name with different auth_env coexist as separate Models — useful when
// one operator runs several subscriptions, but the gateway itself needs no
// special code for that case.
//
// Routing policy (fallback eligibility, circuit breaker) is not part of the
// catalog. It lives in env-driven config and reaches the Service through
// main.go — the catalog's job is data only.
//
// Schema is intentionally flat. Operator-facing notes belong in yaml comments,
// not data fields. There is no apiVersion / kind / metadata wrapping; this is
// a config file, not a CRD. See docs/adr/002-catalog-shape.md.
//
// The default catalog ships at the repo root under catalog/. At runtime
// LLMGATE_CATALOG (a directory path) overrides; otherwise Load reads from
// cwd's ./catalog.
package catalog

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"

	"gopkg.in/yaml.v3"

	"llmgate/internal/domain/llmtypes"
)

const defaultDir = "./catalog"

// Catalog is the runtime view of catalog/ on disk: registered Models keyed
// by lowercased id, plus optional Aliases keyed by lowercased name.
type Catalog struct {
	Models  map[string]*Model
	Aliases map[string]*Alias
}

// Model is one registration. The 6 fields below are exactly what the Service
// needs to route one upstream call. Operator-facing context (description,
// modality, pricing) lives in yaml comments or in external systems — not here.
type Model struct {
	ID         string            `yaml:"id"`
	Vendor     string            `yaml:"vendor"`
	Protocol   llmtypes.Protocol `yaml:"protocol"` // see llmtypes.AllProtocols
	BaseURL    string            `yaml:"base_url"`
	AuthEnv    string            `yaml:"auth_env"`             // env var *name*; empty defaults to LLMGATE_<VENDOR>_API_KEY
	AuthScheme string            `yaml:"auth_scheme"`          // bearer | x-api-key
	ExtraBody  map[string]any    `yaml:"extra_body,omitempty"` // default extra parameters to include in request body
}

// Alias maps a logical name (e.g. "smart") to an ordered list of concrete
// model IDs to try in priority order. Routing tries chain[0] first; on a
// fallback-eligible failure it tries chain[1]; and so on. Raw-model calls
// resolve to a one-element chain, so fallback applies only to alias calls.
type Alias struct {
	Alias string   `yaml:"alias"`
	Chain []string `yaml:"chain"`
}

// LoadDir reads a catalog from the given directory on disk. The directory
// must contain a models/ subdirectory (at minimum); aliases/ is optional.
// All yaml files are parsed strictly — unknown fields fail boot. Filenames
// not ending in .yaml/.yml are ignored, so .example templates coexist
// safely with real entries.
func LoadDir(dir string) (*Catalog, error) {
	return loadFS(os.DirFS(dir))
}

// Load returns the catalog at LLMGATE_CATALOG (a directory path) or from
// cwd's ./catalog when the env is unset.
func Load() (*Catalog, error) {
	dir := os.Getenv("LLMGATE_CATALOG")
	if dir == "" {
		dir = defaultDir
	}
	return LoadDir(dir)
}

func loadFS(fsys fs.FS) (*Catalog, error) {
	cat := &Catalog{
		Models:  make(map[string]*Model),
		Aliases: make(map[string]*Alias),
	}

	if err := walkYAML(fsys, "models", func(name string, data []byte) error {
		var m Model
		if err := decodeStrict(data, &m); err != nil {
			return fmt.Errorf("models/%s: %w", name, err)
		}
		if err := validateModel(&m); err != nil {
			return fmt.Errorf("models/%s: %w", name, err)
		}
		key := strings.ToLower(m.ID)
		if _, exists := cat.Models[key]; exists {
			return fmt.Errorf("catalog: duplicate model id %q (in models/%s)", m.ID, name)
		}
		cat.Models[key] = &m
		return nil
	}); err != nil {
		return nil, err
	}
	if len(cat.Models) == 0 {
		return nil, errors.New("catalog: no models loaded from models/")
	}

	if err := walkYAMLOptional(fsys, "aliases", func(name string, data []byte) error {
		var a Alias
		if err := decodeStrict(data, &a); err != nil {
			return fmt.Errorf("aliases/%s: %w", name, err)
		}
		if err := validateAlias(&a); err != nil {
			return fmt.Errorf("aliases/%s: %w", name, err)
		}
		key := strings.ToLower(a.Alias)
		if _, exists := cat.Models[key]; exists {
			return fmt.Errorf("aliases/%s: alias %q collides with model id of the same name", name, a.Alias)
		}
		if _, exists := cat.Aliases[key]; exists {
			return fmt.Errorf("aliases/%s: duplicate alias %q", name, a.Alias)
		}
		// chain members must reference registered models, and must not repeat.
		seen := make(map[string]struct{}, len(a.Chain))
		for _, m := range a.Chain {
			lc := strings.ToLower(m)
			if _, dup := seen[lc]; dup {
				return fmt.Errorf("aliases/%s: chain has duplicate model %q", name, m)
			}
			seen[lc] = struct{}{}
			if _, ok := cat.Models[lc]; !ok {
				return fmt.Errorf("aliases/%s: alias %q references unknown model %q", name, a.Alias, m)
			}
		}
		cat.Aliases[key] = &Alias{Alias: a.Alias, Chain: append([]string(nil), a.Chain...)}
		return nil
	}); err != nil {
		return nil, err
	}

	return cat, nil
}

// decodeStrict parses yaml with KnownFields(true) so any field not declared
// on the struct fails boot. Catches typos like 'protcol:' or stale fields
// like 'specs:' immediately rather than letting them silently no-op.
func decodeStrict(data []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return err
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
