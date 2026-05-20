package llmresult

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
)

const DefaultAsyncQueueSize = 1000

// AsyncSink decouples result-event production from remote transports. Emit is
// non-blocking: when the bounded queue is full, the event is dropped.
type AsyncSink struct {
	next Sink
	log  *slog.Logger

	mu     sync.RWMutex
	closed bool
	queue  chan *Event
	done   chan struct{}

	closeOnce sync.Once
	closeErr  error
	dropped   atomic.Uint64
}

func NewAsyncSink(next Sink, log *slog.Logger, queueSize int) *AsyncSink {
	if next == nil {
		next = NopSink{}
	}
	if log == nil {
		log = slog.Default()
	}
	if queueSize <= 0 {
		queueSize = DefaultAsyncQueueSize
	}
	s := &AsyncSink{
		next:  next,
		log:   log,
		queue: make(chan *Event, queueSize),
		done:  make(chan struct{}),
	}
	go s.run()
	return s
}

func (s *AsyncSink) Emit(ctx context.Context, event *Event) {
	if s == nil || event == nil {
		return
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		s.recordDrop(ctx, event, "closed")
		return
	}
	select {
	case s.queue <- event:
	default:
		s.recordDrop(ctx, event, "queue_full")
	}
}

func (s *AsyncSink) Close() error {
	if s == nil {
		return nil
	}

	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		close(s.queue)
		s.mu.Unlock()

		<-s.done
		s.closeErr = s.next.Close()
	})
	return s.closeErr
}

func (s *AsyncSink) Dropped() uint64 {
	if s == nil {
		return 0
	}
	return s.dropped.Load()
}

func (s *AsyncSink) run() {
	defer close(s.done)
	for event := range s.queue {
		s.emitOne(event)
	}
}

func (s *AsyncSink) emitOne(event *Event) {
	defer func() {
		if p := recover(); p != nil {
			s.log.LogAttrs(context.Background(), slog.LevelError, "llm result async sink panic",
				slog.String("event_type", eventTypeOf(event)),
				slog.Any("panic", p),
			)
		}
	}()
	s.next.Emit(context.Background(), event)
}

func (s *AsyncSink) recordDrop(ctx context.Context, event *Event, reason string) {
	dropped := s.dropped.Add(1)
	s.log.LogAttrs(ctx, slog.LevelWarn, "llm result event dropped",
		slog.String("event_type", eventTypeOf(event)),
		slog.String("request_id", eventRequestID(event)),
		slog.String("reason", reason),
		slog.Uint64("dropped", dropped),
	)
}

func eventRequestID(event *Event) string {
	if event == nil {
		return ""
	}
	return event.RequestID
}
