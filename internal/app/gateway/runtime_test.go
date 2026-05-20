package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"llmgate/internal/catalog"
	"llmgate/internal/config"
	"llmgate/internal/consumers"
	"llmgate/internal/llmtypes"
)

func TestBuildRuntime_WiresServerAndPublicEndpoints(t *testing.T) {
	t.Setenv("TEST_GATEWAY_OPENAI_KEY", "present")

	store := testConsumerStore(t)
	rt, err := BuildRuntime(context.Background(), RuntimeInput{
		Config: &config.Server{
			Addr:        ":0",
			Environment: "test",
		},
		Catalog: &catalog.Catalog{
			Models: map[string]*catalog.Model{
				"openai-test": {
					ID:         "openai-test",
					Vendor:     "openai",
					Protocol:   llmtypes.ProtocolOpenAI,
					BaseURL:    "https://example.test/v1",
					AuthEnv:    "TEST_GATEWAY_OPENAI_KEY",
					AuthScheme: "bearer",
				},
			},
			Aliases: map[string]*catalog.Alias{
				"smart": {Chain: []string{"openai-test"}},
			},
		},
		Consumers: store,
		Logger:    discardLogger(),
		Version:   "test",
	})
	if err != nil {
		t.Fatalf("BuildRuntime() error = %v", err)
	}
	defer func() { _ = rt.Close() }()

	if rt.Server == nil || rt.Probe == nil {
		t.Fatalf("runtime server/probe = %v/%v, want both set", rt.Server, rt.Probe)
	}

	srv := httptest.NewServer(rt.Server.Handler)
	defer srv.Close()

	for _, path := range []string{"/healthz/live", "/healthz/ready", "/metrics"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s error = %v", path, err)
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			t.Fatalf("GET %s status = %d, want 200", path, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}

func TestRuntimeRun_CancelMarksProbeAndShutsDown(t *testing.T) {
	t.Setenv("TEST_GATEWAY_OPENAI_KEY", "present")

	rt, err := BuildRuntime(context.Background(), RuntimeInput{
		Config: &config.Server{
			Addr:                 "127.0.0.1:0",
			Environment:          "test",
			ShutdownDrainTimeout: time.Second,
		},
		Catalog: &catalog.Catalog{
			Models: map[string]*catalog.Model{
				"openai-test": {
					ID:         "openai-test",
					Vendor:     "openai",
					Protocol:   llmtypes.ProtocolOpenAI,
					BaseURL:    "https://example.test/v1",
					AuthEnv:    "TEST_GATEWAY_OPENAI_KEY",
					AuthScheme: "bearer",
				},
			},
			Aliases: map[string]*catalog.Alias{},
		},
		Consumers: testConsumerStore(t),
		Logger:    discardLogger(),
		Version:   "test",
	})
	if err != nil {
		t.Fatalf("BuildRuntime() error = %v", err)
	}
	defer func() { _ = rt.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rt.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !rt.Probe.IsShuttingDown() {
		t.Fatal("Run() after cancellation must mark readiness as shutting down")
	}
}

func testConsumerStore(t *testing.T) *consumers.Store {
	t.Helper()
	dir := t.TempDir()
	data := []byte("name: test\nkey_hashes:\n  - sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n")
	if err := os.WriteFile(filepath.Join(dir, "test.yaml"), data, 0o600); err != nil {
		t.Fatalf("write consumer fixture: %v", err)
	}
	store, err := consumers.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir() error = %v", err)
	}
	return store
}
