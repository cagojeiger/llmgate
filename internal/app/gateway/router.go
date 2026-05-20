package gateway

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"llmgate/internal/catalog"
	"llmgate/internal/llmrouter"
	"llmgate/internal/llmtypes"
	"llmgate/internal/providers/anthropic"
	"llmgate/internal/providers/openai"
)

// BuildRouterInputs walks the catalog and turns it into the runtime shape the
// Service expects: model id to Provider, alias name to ordered model chain.
func BuildRouterInputs(cat *catalog.Catalog) (llmrouter.Models, llmrouter.Aliases, error) {
	return buildRouterInputs(cat, map[llmtypes.Protocol]providerFactory{
		llmtypes.ProtocolOpenAI:    openaiFactory,
		llmtypes.ProtocolAnthropic: anthropicFactory,
	})
}

// providerFactory builds the Provider for one catalog model. This package
// bridges catalog yaml shape and env-driven credential lookup; llmrouter stays
// catalog-agnostic.
type providerFactory func(*catalog.Model) (llmtypes.Provider, error)

func buildRouterInputs(cat *catalog.Catalog, factories map[llmtypes.Protocol]providerFactory) (llmrouter.Models, llmrouter.Aliases, error) {
	models := make(llmrouter.Models, len(cat.Models))
	for id, m := range cat.Models {
		f, ok := factories[m.Protocol]
		if !ok {
			return nil, nil, fmt.Errorf("no adapter for protocol %q (model %q)", m.Protocol, m.ID)
		}
		p, err := f(m)
		if err != nil {
			return nil, nil, fmt.Errorf("build adapter for model %q protocol %q: %w", m.ID, m.Protocol, err)
		}
		models[id] = p
	}
	aliases := make(llmrouter.Aliases, len(cat.Aliases))
	for name, a := range cat.Aliases {
		aliases[name] = append([]string(nil), a.Chain...)
	}
	return models, aliases, nil
}

func openaiFactory(m *catalog.Model) (llmtypes.Provider, error) {
	apiKey, err := readAuthKey(m)
	if err != nil {
		var missing *missingAuthKeyError
		if errors.As(err, &missing) {
			return missingAuthProviderFor(m, missing.Env), nil
		}
		return nil, err
	}
	return openai.New(openai.Config{
		BaseURL:    m.BaseURL,
		APIKey:     apiKey,
		AuthScheme: m.AuthScheme,
		Name:       m.Vendor,
		ExtraBody:  m.ExtraBody,
	})
}

func anthropicFactory(m *catalog.Model) (llmtypes.Provider, error) {
	apiKey, err := readAuthKey(m)
	if err != nil {
		var missing *missingAuthKeyError
		if errors.As(err, &missing) {
			return missingAuthProviderFor(m, missing.Env), nil
		}
		return nil, err
	}
	return anthropic.New(anthropic.Config{
		BaseURL:    m.BaseURL,
		APIKey:     apiKey,
		AuthScheme: m.AuthScheme,
		Name:       m.Vendor,
	})
}

// readAuthKey resolves the credential env var named by the catalog model.
// When auth_env is omitted in yaml, it defaults to LLMGATE_<VENDOR>_API_KEY.
func readAuthKey(m *catalog.Model) (string, error) {
	envKey := m.AuthEnv
	if envKey == "" {
		envKey = "LLMGATE_" + strings.ToUpper(m.Vendor) + "_API_KEY"
	}
	v := os.Getenv(envKey)
	if v == "" {
		return "", &missingAuthKeyError{Model: m.ID, Env: envKey}
	}
	return v, nil
}

type missingAuthKeyError struct {
	Model string
	Env   string
}

func (e *missingAuthKeyError) Error() string {
	return fmt.Sprintf("model %q: env %s is unset", e.Model, e.Env)
}

type missingAuthProvider struct {
	name  string
	model string
	env   string
}

func missingAuthProviderFor(m *catalog.Model, env string) llmtypes.Provider {
	return &missingAuthProvider{name: m.Vendor, model: m.ID, env: env}
}

func (p *missingAuthProvider) Name() string { return p.name }

func (p *missingAuthProvider) Complete(context.Context, *llmtypes.Request) (*llmtypes.Response, error) {
	return nil, p.err()
}

func (p *missingAuthProvider) CompleteStream(context.Context, *llmtypes.Request) (llmtypes.Stream, error) {
	return nil, p.err()
}

func (p *missingAuthProvider) err() error {
	return &llmtypes.Error{
		Kind:     llmtypes.KindAuth,
		Provider: p.name,
		Message:  fmt.Sprintf("model %q is unavailable because env %s is unset", p.model, p.env),
	}
}
