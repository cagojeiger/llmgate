// Package fake supplies deterministic llmtypes.Provider and llmtypes.Stream
// implementations for tests. Both types accept functional options so each
// test case can shape per-model and global behavior (errors, delays, canned
// events, summaries) without sharing fixture state across packages.
package fake

import (
	"context"
	"io"
	"sync"
	"time"

	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/streaming"
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
// for the given model — exercises the empty-first-event fallback path.
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

// Stream is a deterministic llmtypes.Stream for tests. Recv replays events
// in order, then yields RecvErr (or io.EOF if RecvErr is nil). Close is
// idempotent and increments a counter that tests can inspect via Closed.
type Stream struct {
	events    []*llmtypes.Event
	recvErr   error
	recvDelay time.Duration
	summary   *llmtypes.Summary

	mu        sync.Mutex
	closeOnce sync.Once
	done      chan struct{}
	cursor    int
	closed    int
}

// StreamOption configures a Stream at construction.
type StreamOption func(*Stream)

// NewStream returns a Stream that yields the configured events then EOF.
func NewStream(opts ...StreamOption) *Stream {
	s := &Stream{}
	for _, o := range opts {
		o(s)
	}
	return s
}

// WithEvents queues events to replay in order on successive Recv calls.
func WithEvents(events []*llmtypes.Event) StreamOption {
	return func(s *Stream) { s.events = events }
}

// WithRecvErr makes Recv return err once events are exhausted (instead of
// the default io.EOF).
func WithRecvErr(err error) StreamOption {
	return func(s *Stream) { s.recvErr = err }
}

// WithRecvDelay sleeps d on every Recv call. Used to exercise idle/timeout paths.
func WithRecvDelay(d time.Duration) StreamOption {
	return func(s *Stream) { s.recvDelay = d }
}

// WithSummary returns sum from Stream.Summary after the stream ends.
func WithSummary(sum *llmtypes.Summary) StreamOption {
	return func(s *Stream) { s.summary = sum }
}

// Recv satisfies llmtypes.Stream.
func (s *Stream) Recv() (*llmtypes.Event, error) {
	if s.recvDelay > 0 {
		select {
		case <-time.After(s.recvDelay):
		case <-s.doneChan():
			return nil, io.EOF
		}
	}
	s.mu.Lock()
	if s.cursor < len(s.events) {
		event := s.events[s.cursor]
		s.cursor++
		s.mu.Unlock()
		return event, nil
	}
	s.mu.Unlock()
	if s.recvErr != nil {
		return nil, s.recvErr
	}
	return nil, io.EOF
}

// Close satisfies llmtypes.Stream and is safe under concurrent callers.
func (s *Stream) Close() error {
	done := s.doneChan()
	s.closeOnce.Do(func() { close(done) })
	s.mu.Lock()
	s.closed++
	s.mu.Unlock()
	return nil
}

// Summary returns the configured Summary or an empty one.
func (s *Stream) Summary() *llmtypes.Summary {
	if s.summary == nil {
		return &llmtypes.Summary{}
	}
	return s.summary
}

// Closed reports how many times Close has been called.
func (s *Stream) Closed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Cursor reports how many events Recv has consumed.
func (s *Stream) Cursor() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursor
}

func (s *Stream) doneChan() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done == nil {
		s.done = make(chan struct{})
	}
	return s.done
}
