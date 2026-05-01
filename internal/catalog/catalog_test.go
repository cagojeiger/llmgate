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
	if got := len(cat.Endpoints); got != 2 {
		t.Fatalf("len(Endpoints) = %d, want 2", got)
	}
	if cat.Endpoints["opencode-openai"].APIKey != "test-key" {
		t.Fatalf("openai endpoint APIKey = %q, want test-key", cat.Endpoints["opencode-openai"].APIKey)
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

	// Unknown name (or raw model) returns single-element slice.
	single := cat.ResolveAlias("deepseek-v4-flash")
	if !reflect.DeepEqual(single, []string{"deepseek-v4-flash"}) {
		t.Errorf("ResolveAlias(raw) = %v, want single-element slice", single)
	}
}

func TestLoadFile_AliasUnknownModel(t *testing.T) {
	t.Setenv("TEST_API_KEY", "test-key")
	path := writeCatalog(t, `
vendor: test
base_url: http://example.test/v1
auth_env: TEST_API_KEY
protocols:
  openai:
    auth_scheme: bearer
    models:
      - id: real
aliases:
  bad:
    chain: [real, ghost]
defaults:
  model: real
`)
	_, err := LoadFile(path)
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("error = %v, want unknown-model error referencing ghost", err)
	}
}

func TestLoadFile_AliasCollidesWithModel(t *testing.T) {
	t.Setenv("TEST_API_KEY", "test-key")
	path := writeCatalog(t, `
vendor: test
base_url: http://example.test/v1
auth_env: TEST_API_KEY
protocols:
  openai:
    auth_scheme: bearer
    models:
      - id: real
aliases:
  real:
    chain: [real]
defaults:
  model: real
`)
	_, err := LoadFile(path)
	if err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("error = %v, want alias-collision error", err)
	}
}

func TestLoadFile_AliasEmptyChain(t *testing.T) {
	t.Setenv("TEST_API_KEY", "test-key")
	path := writeCatalog(t, `
vendor: test
base_url: http://example.test/v1
auth_env: TEST_API_KEY
protocols:
  openai:
    auth_scheme: bearer
    models:
      - id: real
aliases:
  blank:
    chain: []
defaults:
  model: real
`)
	_, err := LoadFile(path)
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

func TestLoadFile_DuplicateModel(t *testing.T) {
	t.Setenv("TEST_API_KEY", "test-key")
	path := writeCatalog(t, `
vendor: test
base_url: http://example.test/v1
auth_env: TEST_API_KEY
protocols:
  openai:
    auth_scheme: bearer
    models:
      - id: same
  anthropic:
    auth_scheme: x-api-key
    models:
      - id: same
defaults:
  model: same
`)

	_, err := LoadFile(path)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate model id") {
		t.Fatalf("error = %q, want duplicate model id", err.Error())
	}
}

func TestLoadFile_UnknownDefault(t *testing.T) {
	t.Setenv("TEST_API_KEY", "test-key")
	path := writeCatalog(t, `
vendor: test
base_url: http://example.test/v1
auth_env: TEST_API_KEY
protocols:
  openai:
    auth_scheme: bearer
    models:
      - id: real
defaults:
  model: notreal
`)

	_, err := LoadFile(path)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "defaults.model") {
		t.Fatalf("error = %q, want defaults.model", err.Error())
	}
}

func writeCatalog(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "catalog.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write catalog: %v", err)
	}
	return path
}
