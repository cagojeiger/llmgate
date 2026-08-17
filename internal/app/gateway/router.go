package gateway

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"llmgate/internal/domain/catalog"
	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/routing"
	"llmgate/internal/platform/providers/anthropic"
	"llmgate/internal/platform/providers/openai"
)

// providerFactory builds the chat Provider for one catalog model. This package
// bridges catalog yaml shape and env-driven credential lookup; routing stays
// catalog-agnostic.
type providerFactory func(*catalog.Model) (llmtypes.Provider, error)

// transcriptionProviderFactory is the transcription counterpart of
// providerFactory.
type transcriptionProviderFactory func(*catalog.Model) (llmtypes.TranscriptionProvider, error)

// routerFactories bundles the per-surface factory maps so buildRouterInputs
// takes one argument and tests can substitute either surface independently.
type routerFactories struct {
	chat          map[llmtypes.Protocol]providerFactory
	transcription map[llmtypes.Protocol]transcriptionProviderFactory
}

// defaultProviderFactories is the production factory set. Tests inject a
// substituted set directly into buildRouterInputs.
func defaultProviderFactories() routerFactories {
	return routerFactories{
		chat: map[llmtypes.Protocol]providerFactory{
			llmtypes.ProtocolOpenAI:    openaiFactory,
			llmtypes.ProtocolAnthropic: anthropicFactory,
		},
		transcription: map[llmtypes.Protocol]transcriptionProviderFactory{
			llmtypes.ProtocolOpenAI: openaiTranscriptionFactory,
		},
	}
}

// buildRouterInputs walks the catalog and turns it into the runtime shape the
// Service expects. The model's `api` field selects the surface: chat models
// become routing.Models, transcription models become routing.Transcription
// Models. Aliases are shared across both. Factories let tests inject
// substituted providers per protocol.
func buildRouterInputs(cat *catalog.Catalog, factories routerFactories) (routing.Models, routing.Aliases, routing.TranscriptionModels, error) {
	models := make(routing.Models)
	transcribers := make(routing.TranscriptionModels)
	for id, m := range cat.Models {
		switch m.API {
		case catalog.APIRealtime:
			// Realtime models are not routed through the circuit-breaker
			// Service: the /v1/realtime WS handler resolves them straight from
			// the catalog and brokers the session frame-by-frame. So they build
			// no chat/transcription provider here and are simply skipped.
			continue
		case catalog.APITranscription:
			f, ok := factories.transcription[m.Protocol]
			if !ok {
				return nil, nil, nil, fmt.Errorf("no transcription adapter for protocol %q (model %q)", m.Protocol, m.ID)
			}
			p, err := f(m)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("build transcription adapter for model %q protocol %q: %w", m.ID, m.Protocol, err)
			}
			transcribers[id] = p
		default: // catalog.APIChat (validation defaults empty api to chat)
			f, ok := factories.chat[m.Protocol]
			if !ok {
				return nil, nil, nil, fmt.Errorf("no adapter for protocol %q (model %q)", m.Protocol, m.ID)
			}
			p, err := f(m)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("build adapter for model %q protocol %q: %w", m.ID, m.Protocol, err)
			}
			models[id] = p
		}
	}
	aliases := make(routing.Aliases, len(cat.Aliases))
	for name, a := range cat.Aliases {
		aliases[name] = append([]string(nil), a.Chain...)
	}
	return models, aliases, transcribers, nil
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

// openaiTranscriptionFactory builds a transcription client. Unlike the chat
// factory, a missing credential is not fatal: transcription upstreams (e.g. a
// local STT server) are commonly unauthenticated, so the client is built with
// an empty key and simply sends no Authorization header.
func openaiTranscriptionFactory(m *catalog.Model) (llmtypes.TranscriptionProvider, error) {
	apiKey, err := readAuthKey(m)
	if err != nil {
		var missing *missingAuthKeyError
		if !errors.As(err, &missing) {
			return nil, err
		}
		apiKey = ""
	}
	return openai.NewTranscription(openai.TranscriptionConfig{
		BaseURL:    m.BaseURL,
		APIKey:     apiKey,
		AuthScheme: m.AuthScheme,
		Name:       m.Vendor,
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
