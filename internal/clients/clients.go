// Package clients loads the gateway's caller registration table from a
// directory of yaml files.
//
// Layout (under the root passed to LoadDir):
//
//	<name>.yaml               one yaml per caller — name + key_hashes.
//	                          file basename must match name.
//
// Each yaml registers one Client with one or more sha256 hashes of bearer
// tokens. Raw keys never live on disk; the operator generates a key,
// computes sha256, places the hash in yaml, and hands the raw key to the
// caller out-of-band. Multiple hashes per client enable rotation: add the
// new hash, deploy, retire the old hash on a later deploy.
//
// Schema is intentionally flat (see ADR 008). Operator-facing notes belong
// in yaml comments, not data fields. Permission / quota fields are
// deliberately absent — those are post-processing concerns.
//
// Boot is *closed by default*: missing directory or zero registered clients
// fails boot. There is no "anonymous allowed" mode — operators who want
// open access register a single shared client explicitly.
//
// At runtime LLMGATE_CLIENTS (a directory path) overrides the default; the
// default reads from cwd's ./clients.
package clients

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	defaultDir   = "./clients"
	hashPrefix   = "sha256:"
	hashHexLen   = 64
	keyIDLen     = 8
	maxNameLen   = 64
	envClientDir = "LLMGATE_CLIENTS"
)

// nameRule constrains client names to a safe lowercased identifier shape so
// that audit grep / log fields / future API surfaces all behave predictably.
// First char must be alphanumeric to avoid being mistaken for a CLI flag.
var nameRule = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,` + fmt.Sprint(maxNameLen-1) + `}$`)

// Client is one caller registration. Name is the operator-chosen permanent
// identifier (audit ties facts to this string). KeyHashes are the active
// sha256 hashes of bearer tokens; multiple entries support rotation.
type Client struct {
	Name       string   `yaml:"name"`
	KeyHashes  []string `yaml:"key_hashes"`
}

// Store is the runtime view of the clients directory: an O(1) hash → client
// map for auth lookups, plus a name → client map for completeness.
type Store struct {
	byHash  map[string]*Client
	byName  map[string]*Client
}

// LoadDir reads every *.yaml / *.yml file under dir and builds a Store.
// Strict parsing: unknown fields fail boot. Filename basename must match
// the yaml `name`. Duplicate names or duplicate hashes fail boot. An empty
// or missing directory fails boot — there is no anonymous mode.
func LoadDir(dir string) (*Store, error) {
	return loadFS(os.DirFS(dir), dir)
}

// Load returns the Store at LLMGATE_CLIENTS (a directory path) or from
// cwd's ./clients when the env is unset.
func Load() (*Store, error) {
	dir := os.Getenv(envClientDir)
	if dir == "" {
		dir = defaultDir
	}
	return LoadDir(dir)
}

func loadFS(fsys fs.FS, label string) (*Store, error) {
	store := &Store{
		byHash: make(map[string]*Client),
		byName: make(map[string]*Client),
	}

	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("clients: read %s: %w", label, err)
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
			return nil, fmt.Errorf("clients: read %s/%s: %w", label, name, err)
		}
		var c Client
		if err := decodeStrict(data, &c); err != nil {
			return nil, fmt.Errorf("clients: %s/%s: %w", label, name, err)
		}
		if err := validateClient(&c); err != nil {
			return nil, fmt.Errorf("clients: %s/%s: %w", label, name, err)
		}
		// Filename basename must match the yaml `name` so the operator's
		// "ls" view and audit records line up without rename surprises.
		base := strings.TrimSuffix(strings.TrimSuffix(name, ".yaml"), ".yml")
		if base != c.Name {
			return nil, fmt.Errorf("clients: %s/%s: filename basename %q does not match name %q", label, name, base, c.Name)
		}
		if _, dup := store.byName[c.Name]; dup {
			return nil, fmt.Errorf("clients: duplicate client name %q", c.Name)
		}
		stored := c
		stored.KeyHashes = append([]string(nil), c.KeyHashes...)
		store.byName[c.Name] = &stored

		for _, h := range stored.KeyHashes {
			if existing, dup := store.byHash[h]; dup {
				return nil, fmt.Errorf("clients: duplicate key hash %s shared by %q and %q", shortHash(h), existing.Name, c.Name)
			}
			store.byHash[h] = &stored
		}
	}

	if len(store.byName) == 0 {
		return nil, fmt.Errorf("clients: no clients registered in %s (closed default — register at least one client)", label)
	}
	return store, nil
}

func validateClient(c *Client) error {
	if c.Name == "" {
		return errors.New("name is required")
	}
	if !nameRule.MatchString(c.Name) {
		return fmt.Errorf("name %q must match %s", c.Name, nameRule.String())
	}
	if len(c.KeyHashes) == 0 {
		return fmt.Errorf("client %q: key_hashes must list at least one hash", c.Name)
	}
	seen := make(map[string]struct{}, len(c.KeyHashes))
	for _, h := range c.KeyHashes {
		if err := validateHash(h); err != nil {
			return fmt.Errorf("client %q: %w", c.Name, err)
		}
		if _, dup := seen[h]; dup {
			return fmt.Errorf("client %q: duplicate hash %s", c.Name, shortHash(h))
		}
		seen[h] = struct{}{}
	}
	return nil
}

func validateHash(h string) error {
	if !strings.HasPrefix(h, hashPrefix) {
		return fmt.Errorf("hash %q must start with %q", h, hashPrefix)
	}
	digest := strings.TrimPrefix(h, hashPrefix)
	if len(digest) != hashHexLen {
		return fmt.Errorf("hash %q hex length is %d, want %d", h, len(digest), hashHexLen)
	}
	for _, r := range digest {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		default:
			return fmt.Errorf("hash %q has non-lowercase-hex char %q", h, r)
		}
	}
	return nil
}

// Lookup hashes the raw bearer token and returns the matched client name
// and a short key id (first 8 hex chars of the matched hash) suitable for
// audit. The raw key never appears in the returned values.
func (s *Store) Lookup(rawKey string) (clientName, keyID string, ok bool) {
	if rawKey == "" {
		return "", "", false
	}
	sum := sha256.Sum256([]byte(rawKey))
	full := hashPrefix + hex.EncodeToString(sum[:])
	c, found := s.byHash[full]
	if !found {
		return "", "", false
	}
	return c.Name, hex.EncodeToString(sum[:keyIDLen/2]), true
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
