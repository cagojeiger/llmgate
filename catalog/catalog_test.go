package catalog

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadDefault(t *testing.T) {
	t.Setenv("LLMGATE_OPENCODE_API_KEY", "test-key")

	cat, err := LoadDefault()
	if err != nil {
		t.Fatalf("LoadDefault() error = %v", err)
	}
	if got := len(cat.Models); got != 14 {
		t.Fatalf("len(Models) = %d, want 14", got)
	}
	// One endpoint per model now (1:1) — same vendor + auth_env, but the
	// catalog level keeps each model addressable as its own endpoint so
	// "same model, different key" can coexist later.
	if got := len(cat.Endpoints); got != 14 {
		t.Fatalf("len(Endpoints) = %d, want 14", got)
	}
	if cat.Endpoints["deepseek-v4-flash"].APIKey != "test-key" {
		t.Fatalf("deepseek-v4-flash endpoint APIKey = %q, want test-key", cat.Endpoints["deepseek-v4-flash"].APIKey)
	}
	if cat.Endpoints["minimax-m2.7"].Protocol != "anthropic" {
		t.Fatalf("minimax-m2.7 protocol = %q, want anthropic", cat.Endpoints["minimax-m2.7"].Protocol)
	}
	if cat.Defaults.Model != "deepseek-v4-flash" {
		t.Fatalf("Defaults.Model = %q, want deepseek-v4-flash", cat.Defaults.Model)
	}

	coder, ok := cat.Aliases["coder"]
	if !ok {
		t.Fatal("Aliases[coder] missing")
	}
	wantChain := []string{"deepseek-v4-pro", "deepseek-v4-flash", "kimi-k2.6", "glm-5.1"}
	if !reflect.DeepEqual(coder.Chain, wantChain) {
		t.Fatalf("coder.Chain = %v, want %v", coder.Chain, wantChain)
	}

	if cat.Fallback.CircuitOpen != 30*time.Second {
		t.Errorf("Fallback.CircuitOpen = %v, want 30s", cat.Fallback.CircuitOpen)
	}
	if cat.Fallback.CircuitFailures != 3 {
		t.Errorf("Fallback.CircuitFailures = %d, want 3", cat.Fallback.CircuitFailures)
	}
	if !reflect.DeepEqual(cat.Fallback.OnKinds, []string{"rate_limit", "upstream", "timeout", "network"}) {
		t.Errorf("Fallback.OnKinds = %v, want [rate_limit upstream timeout network]", cat.Fallback.OnKinds)
	}
}

func TestResolveAlias(t *testing.T) {
	t.Setenv("LLMGATE_OPENCODE_API_KEY", "test-key")
	cat, err := LoadDefault()
	if err != nil {
		t.Fatalf("LoadDefault: %v", err)
	}

	chain := cat.ResolveAlias("coder")
	if len(chain) != 4 || chain[0] != "deepseek-v4-pro" {
		t.Errorf("ResolveAlias(coder) = %v, want chain starting with deepseek-v4-pro", chain)
	}
	single := cat.ResolveAlias("deepseek-v4-flash")
	if !reflect.DeepEqual(single, []string{"deepseek-v4-flash"}) {
		t.Errorf("ResolveAlias(raw) = %v, want single-element slice", single)
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

func TestLoadDefault_MissingEnv(t *testing.T) {
	t.Setenv("LLMGATE_OPENCODE_API_KEY", "")

	_, err := LoadDefault()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "LLMGATE_OPENCODE_API_KEY") {
		t.Fatalf("error = %q, want env name", err.Error())
	}
}

func TestLoadDir_DuplicateModel(t *testing.T) {
	t.Setenv("TEST_API_KEY", "test-key")
	// Two yamls share id "same" — the second registration should fail.
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

func TestLoadDir_UnknownDefault(t *testing.T) {
	t.Setenv("TEST_API_KEY", "test-key")
	dir := writeCatalogDir(t,
		map[string]string{"real.yaml": modelYAML("real")},
		map[string]string{"policy.yaml": `defaults:
  model: notreal
`})
	_, err := LoadDir(dir)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "defaults.model") {
		t.Fatalf("error = %q, want defaults.model", err.Error())
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

// writeCatalogDir creates a temp catalog dir with models/ and fallback/
// subdirs populated from the given maps (file name -> yaml body). Pass
// nil for fallback to skip creating that subdir.
func writeCatalogDir(t *testing.T, models map[string]string, fallback map[string]string) string {
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
	if fallback != nil {
		fbDir := filepath.Join(dir, "fallback")
		if err := os.MkdirAll(fbDir, 0o755); err != nil {
			t.Fatalf("mkdir fallback: %v", err)
		}
		for name, body := range fallback {
			if err := os.WriteFile(filepath.Join(fbDir, name), []byte(body), 0o600); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
		}
	}
	return dir
}
