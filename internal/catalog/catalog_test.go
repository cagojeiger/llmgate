package catalog

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// repoCatalogDir points at the repo's actual catalog/ directory from the
// internal/catalog package's working directory at test time.
const repoCatalogDir = "../../catalog"

func TestLoadDir_RepoCatalog(t *testing.T) {
	cat, err := LoadDir(repoCatalogDir)
	if err != nil {
		t.Fatalf("LoadDir(%q) error = %v", repoCatalogDir, err)
	}

	// Self-consistent count: yaml files on disk equal models loaded.
	// A hardcoded number would break every catalog-sync PR (the agent
	// in scripts/catalog_diff/ has no way to update Go test constants).
	modelFiles, _ := filepath.Glob(filepath.Join(repoCatalogDir, "models", "*.yaml"))
	if got, want := len(cat.Models), len(modelFiles); got != want {
		t.Fatalf("len(Models) = %d, want %d (yaml files under %s/models)", got, want, repoCatalogDir)
	}

	// Sampled invariants survive future deletions: skip when the
	// sampled model is gone (catalog-sync removed it), but still catch
	// protocol/auth_env regressions on whichever models remain.
	if m, ok := cat.Models["minimax-m2.7"]; ok && m.Protocol != "anthropic" {
		t.Fatalf("minimax-m2.7 protocol = %q, want anthropic", m.Protocol)
	}
	if m, ok := cat.Models["deepseek-v4-flash"]; ok && m.AuthEnv != "LLMGATE_OPENCODE_API_KEY" {
		t.Fatalf("deepseek-v4-flash auth_env = %q, want LLMGATE_OPENCODE_API_KEY", m.AuthEnv)
	}

	smart, ok := cat.Aliases["smart"]
	if !ok {
		t.Fatal("Aliases[smart] missing")
	}
	if len(smart.Chain) < 1 || smart.Chain[0] != "deepseek-v4-pro" {
		t.Fatalf("smart.Chain = %v, want chain starting with deepseek-v4-pro", smart.Chain)
	}
}

func TestLoadDir_AliasUnknownModel(t *testing.T) {
	dir := writeCatalogDir(t,
		map[string]string{"real.yaml": modelYAML("real")},
		map[string]string{"bad.yaml": `alias: bad
chain: [real, ghost]
`})
	_, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("error = %v, want unknown-model error referencing ghost", err)
	}
}

func TestLoadDir_AliasCollidesWithModel(t *testing.T) {
	dir := writeCatalogDir(t,
		map[string]string{"real.yaml": modelYAML("real")},
		map[string]string{"real.yaml": `alias: real
chain: [real]
`})
	_, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("error = %v, want alias-collision error", err)
	}
}

func TestLoadDir_AliasEmptyChain(t *testing.T) {
	dir := writeCatalogDir(t,
		map[string]string{"real.yaml": modelYAML("real")},
		map[string]string{"blank.yaml": `alias: blank
chain: []
`})
	_, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "chain is empty") {
		t.Fatalf("error = %v, want empty-chain error", err)
	}
}

func TestLoadDir_AliasDuplicateChainEntry(t *testing.T) {
	dir := writeCatalogDir(t,
		map[string]string{"real.yaml": modelYAML("real")},
		map[string]string{"dup.yaml": `alias: dup
chain: [real, real]
`})
	_, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "duplicate model") {
		t.Fatalf("error = %v, want duplicate-chain error", err)
	}
}

func TestLoadDir_DuplicateModel(t *testing.T) {
	dir := writeCatalogDir(t,
		map[string]string{
			"a.yaml": modelYAML("same"),
			"b.yaml": modelYAML("same"),
		},
		nil)
	_, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "duplicate model id") {
		t.Fatalf("error = %v, want duplicate-id error", err)
	}
}

func TestLoadDir_NoModels(t *testing.T) {
	dir := writeCatalogDir(t, map[string]string{}, nil)
	_, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "no models loaded") {
		t.Fatalf("error = %v, want 'no models loaded'", err)
	}
}

func TestLoadDir_AliasesMissingIsOptional(t *testing.T) {
	dir := writeCatalogDir(t, map[string]string{"real.yaml": modelYAML("real")}, nil)

	cat, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(cat.Aliases) != 0 {
		t.Fatalf("len(Aliases) = %d, want 0", len(cat.Aliases))
	}
}

// strict yaml mode: yaml fields not declared on the struct (typos, stale
// 'type:' / 'specs:' / 'notes:' blocks) must fail boot rather than silently
// no-op.
func TestLoadDir_RejectsUnknownField(t *testing.T) {
	dir := writeCatalogDir(t,
		map[string]string{"weird.yaml": `id: weird
vendor: opencode
protocol: openai
base_url: https://example.test/v1
auth_env: TEST_API_KEY
auth_scheme: bearer
specs: { context_window: 128K }
`},
		nil)
	_, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "field specs") {
		t.Fatalf("error = %v, want strict-mode error rejecting 'specs'", err)
	}
}

// 'type:' is the legacy field name we replaced with 'protocol:'. Strict mode
// must catch it explicitly so old yamls don't silently lose their protocol.
func TestLoadDir_RejectsLegacyTypeField(t *testing.T) {
	dir := writeCatalogDir(t,
		map[string]string{"old.yaml": `id: old
vendor: opencode
type: openai
base_url: https://example.test/v1
auth_env: TEST_API_KEY
auth_scheme: bearer
`},
		nil)
	_, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "field type") {
		t.Fatalf("error = %v, want strict-mode error rejecting 'type'", err)
	}
}

func TestLoadDir_BadProtocol(t *testing.T) {
	dir := writeCatalogDir(t,
		map[string]string{"bad.yaml": `id: bad
vendor: x
protocol: grpc
base_url: https://example.test/v1
auth_env: TEST_API_KEY
auth_scheme: bearer
`},
		nil)
	_, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "openai|anthropic") {
		t.Fatalf("error = %v, want protocol enum error", err)
	}
}

func TestLoadDir_BadAuthScheme(t *testing.T) {
	dir := writeCatalogDir(t,
		map[string]string{"bad.yaml": `id: bad
vendor: x
protocol: openai
base_url: https://example.test/v1
auth_env: TEST_API_KEY
auth_scheme: oauth
`},
		nil)
	_, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "bearer|x-api-key") {
		t.Fatalf("error = %v, want auth_scheme enum error", err)
	}
}

func TestLoadDir_BadBaseURL(t *testing.T) {
	dir := writeCatalogDir(t,
		map[string]string{"bad.yaml": `id: bad
vendor: x
protocol: openai
base_url: not a url
auth_env: TEST_API_KEY
auth_scheme: bearer
`},
		nil)
	_, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "http or https") {
		t.Fatalf("error = %v, want base_url scheme error", err)
	}
}

func TestLoadDir_MissingHost(t *testing.T) {
	dir := writeCatalogDir(t,
		map[string]string{"bad.yaml": `id: bad
vendor: x
protocol: openai
base_url: https:///v1
auth_env: TEST_API_KEY
auth_scheme: bearer
`},
		nil)
	_, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "host") {
		t.Fatalf("error = %v, want missing-host error", err)
	}
}

// auth_env is optional in yaml — main.go falls back to LLMGATE_<VENDOR>_API_KEY
// when it's omitted. The catalog loader must accept models without it.
func TestLoadDir_AuthEnvOptional(t *testing.T) {
	dir := writeCatalogDir(t,
		map[string]string{"ok.yaml": `id: ok
vendor: x
protocol: openai
base_url: https://example.test/v1
auth_scheme: bearer
`},
		nil)
	cat, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir error = %v, want nil (auth_env should be optional)", err)
	}
	if got := cat.Models["ok"].AuthEnv; got != "" {
		t.Fatalf("AuthEnv = %q, want empty", got)
	}
}

// .yaml.example files (templates) must coexist alongside real entries.
// The loader globs *.yaml so they should be naturally ignored.
func TestLoadDir_IgnoresExampleSuffix(t *testing.T) {
	dir := t.TempDir()
	modelsDir := filepath.Join(dir, "models")
	if err := os.MkdirAll(modelsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// real entry
	if err := os.WriteFile(filepath.Join(modelsDir, "real.yaml"), []byte(modelYAML("real")), 0o600); err != nil {
		t.Fatalf("write real: %v", err)
	}
	// example template — invalid placeholder content, must NOT be parsed.
	if err := os.WriteFile(filepath.Join(modelsDir, "example.yaml.example"), []byte("garbage: <not-yaml-actually>"), 0o600); err != nil {
		t.Fatalf("write example: %v", err)
	}
	cat, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v (template should have been ignored)", err)
	}
	if len(cat.Models) != 1 {
		t.Fatalf("len(Models) = %d, want 1 (template ignored)", len(cat.Models))
	}
}

func TestLoadFS_AliasesReadErrorFails(t *testing.T) {
	boom := errors.New("boom")
	fsys := readDirErrFS{
		FS:  fstest.MapFS{"models/real.yaml": {Data: []byte(modelYAML("real"))}},
		dir: "aliases",
		err: boom,
	}

	_, err := loadFS(fsys)
	if err == nil {
		t.Fatal("loadFS: expected aliases read error, got nil")
	}
	if !strings.Contains(err.Error(), "catalog: read aliases") || !errors.Is(err, boom) {
		t.Fatalf("error = %v, want wrapped aliases read error", err)
	}
}

// modelYAML returns a minimal valid models/<id>.yaml body. Auth env is
// NOT resolved by the loader — that's the factory's job — so tests can
// reference any env name without setting it.
func modelYAML(id string) string {
	return `id: ` + id + `
vendor: test
protocol: openai
base_url: http://example.test/v1
auth_env: TEST_API_KEY
auth_scheme: bearer
`
}

// writeCatalogDir creates a temp catalog dir laid out as the loader
// expects: models/, aliases/ (optional). Each map is name -> yaml body;
// pass nil/empty to skip aliases/.
func writeCatalogDir(t *testing.T, models, aliases map[string]string) string {
	t.Helper()
	dir := t.TempDir()

	modelsDir := filepath.Join(dir, "models")
	if err := os.MkdirAll(modelsDir, 0o755); err != nil {
		t.Fatalf("mkdir models: %v", err)
	}
	for name, body := range models {
		if err := os.WriteFile(filepath.Join(modelsDir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if aliases != nil {
		aliasesDir := filepath.Join(dir, "aliases")
		if err := os.MkdirAll(aliasesDir, 0o755); err != nil {
			t.Fatalf("mkdir aliases: %v", err)
		}
		for name, body := range aliases {
			if err := os.WriteFile(filepath.Join(aliasesDir, name), []byte(body), 0o600); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
		}
	}
	return dir
}

type readDirErrFS struct {
	fs.FS
	dir string
	err error
}

func (f readDirErrFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name == f.dir {
		return nil, f.err
	}
	return fs.ReadDir(f.FS, name)
}
