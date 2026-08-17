package catalog

import (
	"errors"
	"fmt"
	"net/url"

	"llmgate/internal/domain/llmtypes"
)

func validateModel(m *Model) error {
	if m.ID == "" {
		return errors.New("model: id is required")
	}
	if m.Vendor == "" {
		return fmt.Errorf("model %q: vendor is required", m.ID)
	}
	if m.Protocol == "" {
		return fmt.Errorf("model %q: protocol is required", m.ID)
	}
	if !m.Protocol.Valid() {
		return fmt.Errorf("model %q: protocol %q must be one of %s", m.ID, m.Protocol, llmtypes.JoinProtocols("|"))
	}
	// api defaults to chat so pre-existing model files (which never carried the
	// field) keep validating unchanged. Mutating the pointer here means the
	// resolved value is what routing/gateway later read.
	if m.API == "" {
		m.API = APIChat
	}
	if !m.API.Valid() {
		return fmt.Errorf("model %q: api %q must be one of %s", m.ID, m.API, JoinAPIs("|"))
	}
	if m.BaseURL == "" {
		return fmt.Errorf("model %q: base_url is required", m.ID)
	}
	u, err := url.Parse(m.BaseURL)
	if err != nil {
		return fmt.Errorf("model %q: base_url %q: %w", m.ID, m.BaseURL, err)
	}
	// The realtime surface speaks WebSocket, so its base_url is a ws://|wss://
	// endpoint; every other surface is HTTP. Enforcing per-surface schemes
	// catches a mis-pasted http:// realtime URL (which coder/websocket's Dial
	// would reject far downstream) at boot instead.
	if m.API == APIRealtime {
		if u.Scheme != "ws" && u.Scheme != "wss" {
			return fmt.Errorf("model %q: base_url %q must be ws or wss for realtime api", m.ID, m.BaseURL)
		}
	} else if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("model %q: base_url %q must be http or https", m.ID, m.BaseURL)
	}
	if u.Host == "" {
		return fmt.Errorf("model %q: base_url %q is missing host", m.ID, m.BaseURL)
	}
	// auth_env is optional — main.go defaults to LLMGATE_<VENDOR>_API_KEY
	// when omitted, so most yaml files do not need to repeat it.
	switch m.AuthScheme {
	case "bearer", "x-api-key":
	case "":
		// Transcription and realtime upstreams may be unauthenticated (e.g. a
		// local STT server on the pod network), so an empty scheme is allowed
		// there. Chat models always front a credentialed vendor, so the scheme
		// stays mandatory for them.
		if m.API != APITranscription && m.API != APIRealtime {
			return fmt.Errorf("model %q: auth_scheme is required", m.ID)
		}
	default:
		return fmt.Errorf("model %q: auth_scheme %q must be bearer|x-api-key", m.ID, m.AuthScheme)
	}
	return nil
}

func validateAlias(a *Alias) error {
	if a.Alias == "" {
		return errors.New("alias: name (alias field) is required")
	}
	if len(a.Chain) == 0 {
		return fmt.Errorf("alias %q: chain is empty", a.Alias)
	}
	return nil
}
