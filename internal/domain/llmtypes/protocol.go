package llmtypes

import "strings"

// Protocol is the wire shape an upstream Provider speaks. Each catalog
// model declares one Protocol; app/gateway has exactly one provider factory
// registered per Protocol. Adding a new wire shape (e.g. google) means:
//
//  1. add a new const here
//  2. extend AllProtocols
//  3. register a factory in internal/app/gateway
//
// Catalog yaml decodes into Protocol via the underlying string, so an
// invalid value fails validation at boot rather than at first request.
type Protocol string

const (
	ProtocolOpenAI    Protocol = "openai"
	ProtocolAnthropic Protocol = "anthropic"
)

// AllProtocols lists every accepted Protocol value, in declaration order.
// Catalog validation and operator-facing errors quote this slice so the
// list of valid values lives in one place.
func AllProtocols() []Protocol {
	return []Protocol{ProtocolOpenAI, ProtocolAnthropic}
}

// Valid reports whether p is one of the registered Protocol constants.
func (p Protocol) Valid() bool {
	for _, known := range AllProtocols() {
		if p == known {
			return true
		}
	}
	return false
}

// String satisfies fmt.Stringer.
func (p Protocol) String() string { return string(p) }

// JoinProtocols formats AllProtocols as a comma-separated list for error messages.
func JoinProtocols(sep string) string {
	parts := make([]string, 0, len(AllProtocols()))
	for _, p := range AllProtocols() {
		parts = append(parts, string(p))
	}
	return strings.Join(parts, sep)
}
