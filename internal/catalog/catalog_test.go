package catalog

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

// repoCatalogDir points at the repo's actual catalog/ directory from the
// internal/catalog package's working directory at test time.
const repoCatalogDir = "../../catalog"

func TestLoadDir_RepoCatalog(t *testing.T) {
	t.Setenv("LLMGATE_OPENCODE_API_KEY", "test-key")

	cat, err := LoadDir(repoCatalogDir)
	if err != nil {
		t.Fatalf("LoadDir(%q) error = %v", repoCatalogDir, err)
	}
	if got := len(cat.Models); got != 14 {
		t.Fatalf("len(Models) = %d, want 14", got)
	}
	// One endpoint per model (1:1) — same vendor + auth_env, but each
	// model is addressable as its own endpoint so "same model, different
	// key" can coexist as two yaml files later.
	if got := len(cat.Endpoints); got != 14 {
		t.Fatalf("len(Endpoints) = %d, want 14", got)
	}
	if cat.Endpoints["deepseek-v4-flash"].APIKey != "test-key" {
		t.Fatalf("deepseek-v4-flash endpoint APIKey = %q, want test-key", cat.Endpoints["deepseek-v4-flash"].APIKey)
	}
	if cat.Endpoints["minimax-m2.7"].Protocol != "anthropic" {
		t.Fatalf("minimax-m2.7 protocol = %q, want anthropic", cat.Endpoints["minimax-m2.7"].Protocol)
	}

	coder, ok := cat.Aliases["coder"]
	if !ok {
		t.Fatal("Aliases[coder] missing")
	}
	if len(coder.Chain) < 2 || coder.Chain[0] != "deepseek-v4-pro" || coder.Chain[1] != "deepseek-v4-flash" {
		t.Fatalf("coder.Chain = %v, want chain starting with deepseek-v4-pro, deepseek-v4-flash", coder.Chain)
	}
}

func TestResolveAlias(t *testing.T) {
	t.Setenv("LLMGATE_OPENCODE_API_KEY", "test-key")
	cat, err := LoadDir(repoCatalogDir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}

	chain := cat.ResolveAlias("coder")
	if len(chain) == 0 || chain[0] != "deepseek-v4-pro" {
		t.Errorf("ResolveAlias(coder) = %v, want chain starting with deepseek-v4-pro", chain)
	}
	single := cat.ResolveAlias("deepseek-v4-flash")
	if !reflect.DeepEqual(single, []string{"deepseek-v4-flash"}) {
		t.Errorf("ResolveAlias(raw) = %v, want single-element slice", single)
	}
}

func TestLoadDir_RepoCatalog_MissingEnv(t *testing.T) {
	t.Setenv("LLMGATE_OPENCODE_API_KEY", "")

	_, err := LoadDir(repoCatalogDir)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "LLMGATE_OPENCODE_API_KEY") {
		t.Fatalf("error = %q, want env name", err.Error())
	}
}

func TestLoadDir_AliasUnknownModel(t *testing.T) {
	t.Setenv("TEST_API_KEY", "test-key")
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
	t.Setenv("TEST_API_KEY", "test-key")
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
	t.Setenv("TEST_API_KEY", "test-key")
	dir := writeCatalogDir(t,
		map[string]string{"real.yaml": modelYAML("real")},
		map[string]string{"blank.yaml": `alias: blank
chain: []
`})
	_, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "empty chain") {
		t.Fatalf("error = %v, want empty-chain error", err)
	}
}

func TestLoadDir_DuplicateModel(t *testing.T) {
	t.Setenv("TEST_API_KEY", "test-key")
	dir := writeCatalogDir(t,
		map[string]string{
			"a.yaml": modelYAML("same"),
			"b.yaml": modelYAML("same"),
		},
		nil)
	_, err := LoadDir(dir)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate model id") {
		t.Fatalf("error = %q, want duplicate model id", err.Error())
	}
}

func TestLoadDir_NoModels(t *testing.T) {
	t.Setenv("TEST_API_KEY", "test-key")
	dir := writeCatalogDir(t, map[string]string{}, nil)
	_, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "no models loaded") {
		t.Fatalf("error = %v, want 'no models loaded'", err)
	}
}

func TestLoadDir_AliasesMissingIsOptional(t *testing.T) {
	t.Setenv("TEST_API_KEY", "test-key")
	dir := writeCatalogDir(t, map[string]string{"real.yaml": modelYAML("real")}, nil)

	cat, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(cat.Aliases) != 0 {
		t.Fatalf("len(Aliases) = %d, want 0", len(cat.Aliases))
	}
}

func TestLoadFS_AliasesReadErrorFails(t *testing.T) {
	t.Setenv("TEST_API_KEY", "test-key")
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

// modelYAML returns a minimal valid models/<id>.yaml body using the
// shared TEST_API_KEY env var, so any test that t.Setenv's that var
// can register the model.
func modelYAML(id string) string {
	return `id: ` + id + `
vendor: test
type: openai
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
