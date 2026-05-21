// Package consumers loads the gateway's caller registration table from a
// directory of yaml files.
//
// Layout (under the root passed to LoadDir):
//
//	<name>.yaml               one yaml per caller — name + key_hashes.
//	                          file basename must match name.
//
// Each yaml registers one Consumer with one or more sha256 hashes of bearer
// tokens. Raw keys never live on disk; the operator generates a key,
// computes sha256, places the hash in yaml, and hands the raw key to the
// caller out-of-band. Multiple hashes per consumer enable rotation: add the
// new hash, deploy, retire the old hash on a later deploy.
//
// Schema is intentionally flat (see ADR 003). Operator-facing notes belong
// in yaml comments, not data fields. allowed_aliases is the optional
// coarse-grained model guard; quota / budget fields remain post-processing
// concerns.
//
// Boot is *closed by default*: missing directory or zero registered consumers
// fails boot. There is no "anonymous allowed" mode — operators who want
// open access register a single shared consumer explicitly.
//
// At runtime LLMGATE_CONSUMERS (a directory path) overrides the default; the
// default reads from cwd's ./consumers.
package consumers

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	defaultDir     = "./consumers"
	hashPrefix     = "sha256:"
	hashHexLen     = 64
	keyIDLen       = 8
	maxNameLen     = 64
	envConsumerDir = "LLMGATE_CONSUMERS"
)

// Consumer is one caller registration. Name is the operator-chosen permanent
// identifier (audit ties facts to this string). KeyHashes are the active
// sha256 hashes of bearer tokens; multiple entries support rotation.
// AllowedAliases is an optional allowlist for requested model names, usually
// operator-defined aliases. Empty means unrestricted.
type Consumer struct {
	Name           string   `yaml:"name"`
	KeyHashes      []string `yaml:"key_hashes"`
	AllowedAliases []string `yaml:"allowed_aliases,omitempty"`
}

// Store is the runtime view of the consumers directory: an O(1) hash → consumer
// map for auth lookups, plus a name → consumer map for completeness.
type Store struct {
	byHash map[string]*Consumer
	byName map[string]*Consumer
}

type LookupResult struct {
	Name           string
	KeyID          string
	AllowedAliases []string
}

// LoadDir reads every *.yaml / *.yml file under dir and builds a Store.
// Strict parsing: unknown fields fail boot. Filename basename must match
// the yaml `name`. Duplicate names or duplicate hashes fail boot. An empty
// or missing directory fails boot — there is no anonymous mode.
func LoadDir(dir string) (*Store, error) {
	return loadFS(os.DirFS(dir), dir)
}

// Load returns the Store at LLMGATE_CONSUMERS (a directory path) or from
// cwd's ./consumers when the env is unset.
func Load() (*Store, error) {
	dir := os.Getenv(envConsumerDir)
	if dir == "" {
		dir = defaultDir
	}
	return LoadDir(dir)
}

func loadFS(fsys fs.FS, label string) (*Store, error) {
	store := &Store{
		byHash: make(map[string]*Consumer),
		byName: make(map[string]*Consumer),
	}

	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("consumers: read %s: %w", label, err)
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("consumers: read %s/%s: %w", label, name, err)
		}
		var c Consumer
		if err := decodeStrict(data, &c); err != nil {
			return nil, fmt.Errorf("consumers: %s/%s: %w", label, name, err)
		}
		if err := validateConsumer(&c); err != nil {
			return nil, fmt.Errorf("consumers: %s/%s: %w", label, name, err)
		}
		// Filename basename must match the yaml `name` so the operator's
		// "ls" view and audit records line up without rename surprises.
		base := strings.TrimSuffix(strings.TrimSuffix(name, ".yaml"), ".yml")
		if base != c.Name {
			return nil, fmt.Errorf("consumers: %s/%s: filename basename %q does not match name %q", label, name, base, c.Name)
		}
		if _, dup := store.byName[c.Name]; dup {
			return nil, fmt.Errorf("consumers: duplicate consumer name %q", c.Name)
		}
		stored := c
		stored.KeyHashes = append([]string(nil), c.KeyHashes...)
		stored.AllowedAliases = append([]string(nil), c.AllowedAliases...)
		store.byName[c.Name] = &stored

		for _, h := range stored.KeyHashes {
			if existing, dup := store.byHash[h]; dup {
				return nil, fmt.Errorf("consumers: duplicate key hash %s shared by %q and %q", shortHash(h), existing.Name, c.Name)
			}
			store.byHash[h] = &stored
		}
	}

	if len(store.byName) == 0 {
		return nil, fmt.Errorf("consumers: no consumers registered in %s (closed default — register at least one consumer)", label)
	}
	return store, nil
}

// Len returns the number of registered consumers (one entry per yaml
// file). Useful for boot-time logging and tests; no auth path consults
// it.
func (s *Store) Len() int {
	return len(s.byName)
}

// Lookup hashes the raw bearer token and returns the matched consumer name
// and a short key id (first 8 hex chars of the matched hash) suitable for
// audit. The raw key never appears in the returned values.
func (s *Store) Lookup(rawKey string) (consumerName, keyID string, ok bool) {
	info, ok := s.LookupInfo(rawKey)
	if !ok {
		return "", "", false
	}
	return info.Name, info.KeyID, true
}

// LookupInfo returns the matched consumer identity and optional policy fields.
// The returned slices are copies so auth callers cannot mutate the Store.
func (s *Store) LookupInfo(rawKey string) (LookupResult, bool) {
	if s == nil || rawKey == "" {
		return LookupResult{}, false
	}
	sum := sha256.Sum256([]byte(rawKey))
	full := hashPrefix + hex.EncodeToString(sum[:])
	c, found := s.byHash[full]
	if !found {
		return LookupResult{}, false
	}
	return LookupResult{
		Name:           c.Name,
		KeyID:          hex.EncodeToString(sum[:keyIDLen/2]),
		AllowedAliases: append([]string(nil), c.AllowedAliases...),
	}, true
}

// shortHash trims a stored "sha256:..." string to the audit-safe first 8
// hex chars (no prefix), matching what Lookup returns to the caller.
func shortHash(h string) string {
	digest := strings.TrimPrefix(h, hashPrefix)
	if len(digest) > keyIDLen {
		return digest[:keyIDLen]
	}
	return digest
}

func decodeStrict(data []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	return dec.Decode(out)
}
