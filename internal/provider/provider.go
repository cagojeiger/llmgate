package provider

import "context"

// Provider is the spine of llmgate. Every upstream — OpenCode Go today,
// OpenRouter or anthropic-direct later — implements this. The HTTP server
// and the probe CLI both depend on this and nothing else.
type Provider interface {
	Name() string
	Complete(ctx context.Context, req *Request) (*Response, error)
}
