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

// repoConsumersDir points at the repo's consumers/ directory from the
// internal/consumers package's working directory at test time.
const repoConsumersDir = "../../consumers"

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

func TestLoadDir_MissingDir(t *testing.T) {
	_, err := LoadDir(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("LoadDir on missing dir = nil, want error")
	}
}

func TestLoadDir_EmptyDir(t *testing.T) {
	_, err := LoadDir(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "no consumers registered") {
		t.Fatalf("error = %v, want no-consumers-registered error", err)
	}
}

func TestLoadDir_StrictUnknownField(t *testing.T) {
	dir := writeClientDir(t, map[string]string{
		"alpha.yaml": "name: alpha\nkey_hashes:\n  - " + sha256Hash("k") + "\nallowed_aliases: [smart]\n",
	})
	_, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "allowed_aliases") {
		t.Fatalf("error = %v, want strict-parse error mentioning allowed_aliases", err)
	}
}

func TestLoadDir_FilenameNameMismatch(t *testing.T) {
	dir := writeClientDir(t, map[string]string{
		"alpha.yaml": "name: bravo\nkey_hashes:\n  - " + sha256Hash("k") + "\n",
	})
	_, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error = %v, want filename-mismatch error", err)
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

func TestLoadDir_EmptyKeyHashes(t *testing.T) {
	dir := writeClientDir(t, map[string]string{
		"alpha.yaml": "name: alpha\nkey_hashes: []\n",
	})
	_, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "key_hashes") {
		t.Fatalf("error = %v, want empty-key_hashes error", err)
	}
}

func TestLoadDir_BadHashFormat(t *testing.T) {
	cases := map[string]string{
		"missing-prefix": sha256Hex("k"),                         // no "sha256:" prefix
		"wrong-prefix":   "md5:" + sha256Hex("k"),                // unknown algo
		"short-hex":      hashPrefix + "abcd",                    // too short
		"upper-hex":      hashPrefix + strings.ToUpper(sha256Hex("k")), // uppercase hex
		"non-hex":        hashPrefix + strings.Repeat("z", 64),   // non-hex chars
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

func TestLoadDir_DuplicateNameAcrossFiles(t *testing.T) {
	// Same name in two different files (different filenames so filename-name
	// check passes individually). LoadDir should still catch the dup.
	rawA := "rawA"
	rawB := "rawB"
	dir := writeClientDir(t, map[string]string{
		"alpha.yaml": "name: alpha\nkey_hashes:\n  - " + sha256Hash(rawA) + "\n",
		"alpha.yml":  "name: alpha\nkey_hashes:\n  - " + sha256Hash(rawB) + "\n",
	})
	_, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "duplicate consumer name") {
		t.Fatalf("error = %v, want duplicate-name error", err)
	}
}

func TestLoadDir_DuplicateHashAcrossClients(t *testing.T) {
	shared := sha256Hash("shared-secret")
	dir := writeClientDir(t, map[string]string{
		"alpha.yaml": "name: alpha\nkey_hashes:\n  - " + shared + "\n",
		"bravo.yaml": "name: bravo\nkey_hashes:\n  - " + shared + "\n",
	})
	_, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "duplicate key hash") {
		t.Fatalf("error = %v, want duplicate-hash error", err)
	}
}

func TestLoadDir_DuplicateHashWithinClient(t *testing.T) {
	dup := sha256Hash("k")
	dir := writeClientDir(t, map[string]string{
		"alpha.yaml": "name: alpha\nkey_hashes:\n  - " + dup + "\n  - " + dup + "\n",
	})
	_, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "duplicate hash") {
		t.Fatalf("error = %v, want intra-consumer duplicate-hash error", err)
	}
}

func TestLoadDir_IgnoresNonYamlFiles(t *testing.T) {
	// .example template should not break boot.
	dir := writeClientDir(t, map[string]string{
		"alpha.yaml":   "name: alpha\nkey_hashes:\n  - " + sha256Hash("k") + "\n",
		"README.md":    "Operator notes — not a consumer.",
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
