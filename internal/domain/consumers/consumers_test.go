package consumers

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func sha256Hash(s string) string {
	return hashPrefix + sha256Hex(s)
}

// writeClientDir writes the named yaml entries into a fresh temp dir and
// returns its path. Keys of files map to filenames; values are file body.
func writeClientDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return dir
}

// repoConsumersDir points at the repo's consumers/ directory from this
// package's working directory at test time.
const repoConsumersDir = "../../../consumers"

func TestLoadDir_RepoConsumers(t *testing.T) {
	store, err := LoadDir(repoConsumersDir)
	if err != nil {
		t.Fatalf("LoadDir(%q) error = %v", repoConsumersDir, err)
	}
	if got := len(store.byName); got == 0 {
		t.Fatalf("len(byName) = 0, want at least one example registration")
	}
	name, keyID, ok := store.Lookup("example-key-001")
	if !ok || name != "example" {
		t.Fatalf("Lookup(example-key-001) = (%q, %q, %v), want (example, _, true)", name, keyID, ok)
	}
	if _, _, ok := store.Lookup("example-key-002"); !ok {
		t.Fatalf("Lookup(example-key-002) miss — rotation second-key fixture absent")
	}
	if _, _, ok := store.Lookup("not-issued"); ok {
		t.Fatal("Lookup(unknown) hit, want miss")
	}
}

func TestLoadDir_HappyPath(t *testing.T) {
	rawA := "raw-key-A"
	rawB := "raw-key-B"
	dir := writeClientDir(t, map[string]string{
		"alpha.yaml": "name: alpha\nkey_hashes:\n  - " + sha256Hash(rawA) + "\n  - " + sha256Hash(rawB) + "\n",
	})
	store, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir error = %v", err)
	}
	if len(store.byName) != 1 {
		t.Fatalf("byName len = %d, want 1", len(store.byName))
	}
	if c := store.byName["alpha"]; c == nil || len(c.KeyHashes) != 2 {
		t.Fatalf("alpha registration missing or wrong key count: %+v", c)
	}

	name, keyID, ok := store.Lookup(rawA)
	if !ok || name != "alpha" {
		t.Fatalf("Lookup(rawA) = (%q, %q, %v), want (alpha, _, true)", name, keyID, ok)
	}
	if len(keyID) != keyIDLen {
		t.Fatalf("keyID = %q (len %d), want len %d", keyID, len(keyID), keyIDLen)
	}
	wantPrefix := sha256Hex(rawA)[:keyIDLen]
	if keyID != wantPrefix {
		t.Fatalf("keyID = %q, want %q", keyID, wantPrefix)
	}

	name, _, ok = store.Lookup(rawB)
	if !ok || name != "alpha" {
		t.Fatalf("Lookup(rawB) = (%q, _, %v), want (alpha, _, true)", name, ok)
	}

	if _, _, ok := store.Lookup("not-a-real-key"); ok {
		t.Fatalf("Lookup(unknown) = ok, want miss")
	}
	if _, _, ok := store.Lookup(""); ok {
		t.Fatalf("Lookup(\"\") = ok, want miss (empty bearer rejected)")
	}
}

// TestLoadDir_Errors covers the eight failure modes LoadDir must
// reject with an informative error. Adding a new failure case is one
// struct entry — no new test function, no test name to invent, no
// setup duplication. wantSubstr lets each case assert the operator-
// facing diagnostic without being brittle to exact wording.
func TestLoadDir_Errors(t *testing.T) {
	cases := []struct {
		name       string
		files      map[string]string // nil ⇒ dir doesn't exist on disk
		wantSubstr string             // empty ⇒ any error is fine
	}{
		{
			name:  "missing dir",
			files: nil,
		},
		{
			name:       "empty dir",
			files:      map[string]string{},
			wantSubstr: "no consumers registered",
		},
		{
			name: "strict unknown field",
			files: map[string]string{
				"alpha.yaml": "name: alpha\nkey_hashes:\n  - " + sha256Hash("k") + "\nquota: 10\n",
			},
			wantSubstr: "quota",
		},
		{
			name: "filename does not match name field",
			files: map[string]string{
				"alpha.yaml": "name: bravo\nkey_hashes:\n  - " + sha256Hash("k") + "\n",
			},
			wantSubstr: "does not match",
		},
		{
			name: "empty key_hashes",
			files: map[string]string{
				"alpha.yaml": "name: alpha\nkey_hashes: []\n",
			},
			wantSubstr: "key_hashes",
		},
		{
			name: "duplicate name across files",
			files: map[string]string{
				"alpha.yaml": "name: alpha\nkey_hashes:\n  - " + sha256Hash("rawA") + "\n",
				"alpha.yml":  "name: alpha\nkey_hashes:\n  - " + sha256Hash("rawB") + "\n",
			},
			wantSubstr: "duplicate consumer name",
		},
		{
			name: "duplicate hash across clients",
			files: map[string]string{
				"alpha.yaml": "name: alpha\nkey_hashes:\n  - " + sha256Hash("shared-secret") + "\n",
				"bravo.yaml": "name: bravo\nkey_hashes:\n  - " + sha256Hash("shared-secret") + "\n",
			},
			wantSubstr: "duplicate key hash",
		},
		{
			name: "duplicate hash within one client",
			files: map[string]string{
				"alpha.yaml": "name: alpha\nkey_hashes:\n  - " + sha256Hash("k") + "\n  - " + sha256Hash("k") + "\n",
			},
			wantSubstr: "duplicate hash",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var dir string
			if tc.files == nil {
				dir = filepath.Join(t.TempDir(), "does-not-exist")
			} else {
				dir = writeClientDir(t, tc.files)
			}
			_, err := LoadDir(dir)
			if err == nil {
				t.Fatalf("LoadDir = nil error, want error (substr=%q)", tc.wantSubstr)
			}
			if tc.wantSubstr != "" && !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("error = %v, want substring %q", err, tc.wantSubstr)
			}
		})
	}
}

func TestLoadDir_AllowedAliases(t *testing.T) {
	raw := "raw-key"
	dir := writeClientDir(t, map[string]string{
		"alpha.yaml": "name: alpha\nkey_hashes:\n  - " + sha256Hash(raw) + "\nallowed_aliases:\n  - cheap\n  - worker\n  - qwen3.6-plus\n",
	})
	store, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir error = %v", err)
	}
	c := store.byName["alpha"]
	if c == nil {
		t.Fatal("alpha registration missing")
	}
	want := []string{"cheap", "worker", "qwen3.6-plus"}
	if strings.Join(c.AllowedAliases, ",") != strings.Join(want, ",") {
		t.Fatalf("AllowedAliases = %#v, want %#v", c.AllowedAliases, want)
	}

	info, ok := store.LookupInfo(raw)
	if !ok {
		t.Fatal("LookupInfo(raw) miss")
	}
	if strings.Join(info.AllowedAliases, ",") != strings.Join(want, ",") {
		t.Fatalf("LookupInfo AllowedAliases = %#v, want %#v", info.AllowedAliases, want)
	}
	info.AllowedAliases[0] = "mutated"
	if store.byName["alpha"].AllowedAliases[0] != "cheap" {
		t.Fatalf("LookupInfo returned store-owned slice: %#v", store.byName["alpha"].AllowedAliases)
	}
}

func TestLoadDir_InvalidAllowedAliases(t *testing.T) {
	cases := map[string]string{
		"empty":     `""`,
		"upper":     "Smart",
		"slash":     "smart/pro",
		"space":     "smart pro",
		"duplicate": "cheap\n  - cheap",
	}
	for label, aliases := range cases {
		t.Run(label, func(t *testing.T) {
			dir := writeClientDir(t, map[string]string{
				"alpha.yaml": "name: alpha\nkey_hashes:\n  - " + sha256Hash("k") + "\nallowed_aliases:\n  - " + aliases + "\n",
			})
			if _, err := LoadDir(dir); err == nil {
				t.Fatalf("expected error for allowed_aliases case %q", label)
			}
		})
	}
}

func TestLoadDir_InvalidName(t *testing.T) {
	cases := map[string]string{
		"empty":      "",
		"upper":      "Alpha",
		"slash":      "a/b",
		"colon":      "a:b",
		"dot":        "a.b",
		"space":      "a b",
		"hyphen-1st": "-alpha",
		"too-long":   strings.Repeat("a", 65),
	}
	for label, badName := range cases {
		t.Run(label, func(t *testing.T) {
			file := badName + ".yaml"
			if badName == "" || strings.ContainsAny(badName, "/") {
				// names that can't be filenames — use neutral filename
				// and let the yaml-name vs filename check be the failure.
				file = "neutral.yaml"
			}
			dir := writeClientDir(t, map[string]string{
				file: "name: " + badName + "\nkey_hashes:\n  - " + sha256Hash("k") + "\n",
			})
			if _, err := LoadDir(dir); err == nil {
				t.Fatalf("expected error for invalid name %q", badName)
			}
		})
	}
}

func TestLoadDir_BadHashFormat(t *testing.T) {
	cases := map[string]string{
		"missing-prefix": sha256Hex("k"),                               // no "sha256:" prefix
		"wrong-prefix":   "md5:" + sha256Hex("k"),                      // unknown algo
		"short-hex":      hashPrefix + "abcd",                          // too short
		"upper-hex":      hashPrefix + strings.ToUpper(sha256Hex("k")), // uppercase hex
		"non-hex":        hashPrefix + strings.Repeat("z", 64),         // non-hex chars
	}
	for label, hashStr := range cases {
		t.Run(label, func(t *testing.T) {
			dir := writeClientDir(t, map[string]string{
				"alpha.yaml": "name: alpha\nkey_hashes:\n  - " + hashStr + "\n",
			})
			if _, err := LoadDir(dir); err == nil {
				t.Fatalf("expected error for bad hash %q", hashStr)
			}
		})
	}
}

func TestLoadDir_IgnoresNonYamlFiles(t *testing.T) {
	// .example template should not break boot.
	dir := writeClientDir(t, map[string]string{
		"alpha.yaml":     "name: alpha\nkey_hashes:\n  - " + sha256Hash("k") + "\n",
		"README.md":      "Operator notes — not a consumer.",
		"sample.example": "name: sample\nkey_hashes: []\n",
	})
	store, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir error = %v", err)
	}
	if len(store.byName) != 1 {
		t.Fatalf("byName len = %d, want 1 (non-yaml files must be skipped)", len(store.byName))
	}
}

func TestLoad_EnvOverride(t *testing.T) {
	dir := writeClientDir(t, map[string]string{
		"alpha.yaml": "name: alpha\nkey_hashes:\n  - " + sha256Hash("k") + "\n",
	})
	t.Setenv(envConsumerDir, dir)
	store, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if _, _, ok := store.Lookup("k"); !ok {
		t.Fatal("Lookup after env-overridden Load failed")
	}
}
