package fake

import (
	"io"
	"sync"
	"time"

	"llmgate/internal/domain/llmtypes"
)

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
