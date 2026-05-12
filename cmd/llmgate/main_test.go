package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"llmgate/internal/catalog"
	"llmgate/internal/llmrouter"
	"llmgate/internal/llmtypes"
)

func TestBuildRouterInputs_MissingAuthDoesNotBlockOtherModels(t *testing.T) {
	t.Setenv("TEST_OPENCODE_KEY", "present")
	t.Setenv("TEST_OPENROUTER_KEY", "")

	cat := &catalog.Catalog{
		Models: map[string]*catalog.Model{
			"opencode-ok": {
				ID:         "opencode-ok",
				Vendor:     "opencode",
				Protocol:   "openai",
				BaseURL:    "https://opencode.example/v1",
				AuthEnv:    "TEST_OPENCODE_KEY",
				AuthScheme: "bearer",
			},
			"openrouter-missing": {
				ID:         "openrouter-missing",
				Vendor:     "openrouter",
				Protocol:   "openai",
				BaseURL:    "https://openrouter.example/api/v1",
				AuthEnv:    "TEST_OPENROUTER_KEY",
				AuthScheme: "bearer",
			},
		},
		Aliases: map[string]*catalog.Alias{},
	}

	models, aliases, err := buildRouterInputs(cat, map[llmtypes.Protocol]providerFactory{
		"openai": openaiFactory,
	})
	if err != nil {
		t.Fatalf("buildRouterInputs() error = %v", err)
	}
	if got := models["opencode-ok"].Name(); got != "opencode" {
		t.Fatalf("opencode provider name = %q, want opencode", got)
	}
	svc, err := llmrouter.NewService(models, aliases, llmrouter.FallbackPolicy{}, slog.Default())
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}

	_, err = svc.Complete(context.Background(), &llmtypes.Request{
		Model:    "openrouter-missing",
		Messages: []llmtypes.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("Complete() error = nil, want missing auth error")
	}
	var perr *llmtypes.Error
	if !strings.Contains(err.Error(), "TEST_OPENROUTER_KEY") || !errors.As(err, &perr) {
		t.Fatalf("error = %v, want llmtypes auth error mentioning TEST_OPENROUTER_KEY", err)
	}
	if perr.Kind != llmtypes.KindAuth || perr.Provider != "openrouter" {
		t.Fatalf("error kind/provider = %q/%q, want auth/openrouter", perr.Kind, perr.Provider)
	}
}
