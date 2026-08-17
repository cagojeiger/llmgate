package catalog

import "strings"

// API is the gateway-domain invocation surface a model serves. It decides
// which provider kind app/gateway builds for the model and which HTTP route
// can reach it: chat models answer /v1/chat/completions, transcription models
// answer /v1/audio/transcriptions. It is deliberately separate from Protocol
// (the wire dialect) — a single protocol like openai speaks both surfaces.
type API string

const (
	// APIChat is the default surface; a model with no `api:` field is chat.
	APIChat API = "chat"
	// APITranscription is the speech-to-text surface.
	APITranscription API = "transcription"
	// APIRealtime is the WebSocket realtime-transcription surface. Unlike chat
	// and transcription (both HTTP), a realtime model registers a ws://|wss://
	// base_url and is brokered by /v1/realtime rather than routed through the
	// circuit-breaker Service — the gateway relays event frames verbatim.
	APIRealtime API = "realtime"
)

// allAPIs lists every accepted API value, in declaration order, so
// validation and operator-facing errors quote one source of truth.
func allAPIs() []API {
	return []API{APIChat, APITranscription, APIRealtime}
}

// Valid reports whether a is one of the registered API constants.
func (a API) Valid() bool {
	for _, known := range allAPIs() {
		if a == known {
			return true
		}
	}
	return false
}

// String satisfies fmt.Stringer.
func (a API) String() string { return string(a) }

// JoinAPIs formats allAPIs as a sep-separated list for error messages.
func JoinAPIs(sep string) string {
	parts := make([]string, 0, len(allAPIs()))
	for _, a := range allAPIs() {
		parts = append(parts, string(a))
	}
	return strings.Join(parts, sep)
}
