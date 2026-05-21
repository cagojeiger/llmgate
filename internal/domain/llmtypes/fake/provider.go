package fake

import (
	"context"
	"sync"
	"time"

	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/streaming"
)

// Provider is a deterministic llmtypes.Provider for tests.
type Provider struct {
	mu sync.Mutex

	name string

	completeErrors  map[string]*llmtypes.Error
	completeError   *llmtypes.Error
	completeDelays  map[string]time.Duration
	completeBuilder func(*llmtypes.Request) *llmtypes.Response

	streamErrors  map[string]*llmtypes.Error
	streamError   *llmtypes.Error
	streamDelays  map[string]time.Duration
	streamRecv    map[string]time.Duration
	streamEmpty   map[string]bool
	streamBuilder func(*llmtypes.Request) []*llmtypes.Event

	completeCalls int
	streamCalls   int
	lastComplete  *llmtypes.Request
	lastStream    *llmtypes.Request
}

// Option configures a Provider at construction.
type Option func(*Provider)

// NewProvider returns a Provider that succeeds by default with a canned
// "ok" response on Complete and a single-chunk stream on CompleteStream.
// Options override per-model and global behavior.
func NewProvider(name string, opts ...Option) *Provider {
	p := &Provider{
		name:           name,
		completeErrors: map[string]*llmtypes.Error{},
		completeDelays: map[string]time.Duration{},
		streamErrors:   map[string]*llmtypes.Error{},
		streamDelays:   map[string]time.Duration{},
		streamRecv:     map[string]time.Duration{},
		streamEmpty:    map[string]bool{},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// WithCompleteError fails every Complete call with err.
func WithCompleteError(err *llmtypes.Error) Option {
	return func(p *Provider) { p.completeError = err }
}

// WithCompleteErrorOnModel fails Complete with err only when the request
// targets the given model id. Per-model takes precedence over global.
func WithCompleteErrorOnModel(model string, err *llmtypes.Error) Option {
	return func(p *Provider) { p.completeErrors[model] = err }
}

// WithCompleteDelay sleeps d before returning from Complete on the given model.
// The sleep honors ctx cancel.
func WithCompleteDelay(model string, d time.Duration) Option {
	return func(p *Provider) { p.completeDelays[model] = d }
}

// WithCompleteResponse replaces the canned success response with one built
// per request. Ignored when an error option matches.
func WithCompleteResponse(fn func(*llmtypes.Request) *llmtypes.Response) Option {
	return func(p *Provider) { p.completeBuilder = fn }
}

// WithStreamError fails every CompleteStream call with err (pre-stream).
func WithStreamError(err *llmtypes.Error) Option {
	return func(p *Provider) { p.streamError = err }
}

// WithStreamErrorOnModel fails CompleteStream with err only on the given model.
func WithStreamErrorOnModel(model string, err *llmtypes.Error) Option {
	return func(p *Provider) { p.streamErrors[model] = err }
}

// WithStreamDelay sleeps d before returning from CompleteStream on the given model.
func WithStreamDelay(model string, d time.Duration) Option {
	return func(p *Provider) { p.streamDelays[model] = d }
}

// WithStreamRecvDelay sleeps d on every Recv call from streams produced for
// the given model. Used to exercise idle/timeout paths.
func WithStreamRecvDelay(model string, d time.Duration) Option {
	return func(p *Provider) { p.streamRecv[model] = d }
}

// WithStreamEmptyEOFOnModel makes CompleteStream emit zero events before EOF
// for the given model; exercises the empty-first-event fallback path.
func WithStreamEmptyEOFOnModel(model string) Option {
	return func(p *Provider) { p.streamEmpty[model] = true }
}

// WithStreamEvents replaces the canned single-chunk stream with events
// built per request.
func WithStreamEvents(fn func(*llmtypes.Request) []*llmtypes.Event) Option {
	return func(p *Provider) { p.streamBuilder = fn }
}

// Name reports the provider name (matches llmtypes.Provider).
func (p *Provider) Name() string { return p.name }

// Complete satisfies llmtypes.Provider.
func (p *Provider) Complete(ctx context.Context, req *llmtypes.Request) (*llmtypes.Response, error) {
	p.mu.Lock()
	p.completeCalls++
	p.lastComplete = req
	p.mu.Unlock()

	if d := p.completeDelays[req.Model]; d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if e, ok := p.completeErrors[req.Model]; ok {
		return nil, e
	}
	if p.completeError != nil {
		return nil, p.completeError
	}
	if p.completeBuilder != nil {
		return p.completeBuilder(req), nil
	}
	return &llmtypes.Response{
		Model: req.Model,
		Choices: []llmtypes.Choice{{
			Index:   0,
			Message: llmtypes.Message{Role: "assistant", Content: "ok"},
		}},
	}, nil
}

// CompleteStream satisfies llmtypes.Provider. The returned Stream replays
// the canned events; first-event validation runs through streaming.ValidateStreamStart
// so adapter-shape behavior matches production paths.
func (p *Provider) CompleteStream(ctx context.Context, req *llmtypes.Request) (llmtypes.Stream, error) {
	p.mu.Lock()
	p.streamCalls++
	p.lastStream = req
	p.mu.Unlock()

	if d := p.streamDelays[req.Model]; d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if e, ok := p.streamErrors[req.Model]; ok {
		return nil, e
	}
	if p.streamError != nil {
		return nil, p.streamError
	}

	var events []*llmtypes.Event
	switch {
	case p.streamEmpty[req.Model]:
		events = nil
	case p.streamBuilder != nil:
		events = p.streamBuilder(req)
	default:
		events = []*llmtypes.Event{
			{Choices: []llmtypes.ChoiceDelta{{Delta: llmtypes.Delta{Content: "ok"}}}},
		}
	}

	raw := &Stream{events: events, recvDelay: p.streamRecv[req.Model]}
	return streaming.ValidateStreamStart(ctx, raw)
}

// CompleteCalls returns the number of Complete invocations seen.
func (p *Provider) CompleteCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.completeCalls
}

// StreamCalls returns the number of CompleteStream invocations seen.
func (p *Provider) StreamCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.streamCalls
}

// LastCompleteRequest returns the most recent Request seen by Complete.
func (p *Provider) LastCompleteRequest() *llmtypes.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastComplete
}

// LastStreamRequest returns the most recent Request seen by CompleteStream.
func (p *Provider) LastStreamRequest() *llmtypes.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastStream
}
