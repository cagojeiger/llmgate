package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
