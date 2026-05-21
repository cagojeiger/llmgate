package sink

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	llmresult "llmgate/internal/domain/llmresult/schema"
)

const (
	defaultAsyncQueueSize     = 1000
	defaultAsyncBatchSize     = 100
	defaultAsyncFlushInterval = time.Second
)

type AsyncConfig struct {
	QueueSize     int
	BatchSize     int
	FlushInterval time.Duration
}

// AsyncSink decouples result-event production from remote transports. Emit is
// non-blocking: when the bounded queue is full, the event is dropped.
type AsyncSink struct {
	next Sink
	log  *slog.Logger

	mu            sync.RWMutex
	closed        bool
	queue         chan *llmresult.Event
	done          chan struct{}
	batchSize     int
	flushInterval time.Duration

	closeOnce sync.Once
	closeErr  error
	dropped   atomic.Uint64
}

func NewAsyncSinkWithConfig(next Sink, log *slog.Logger, cfg AsyncConfig) *AsyncSink {
	if next == nil {
		next = NopSink{}
	}
	if log == nil {
		log = slog.Default()
	}
	cfg = cfg.withDefaults()
	s := &AsyncSink{
		next:          next,
		log:           log,
		queue:         make(chan *llmresult.Event, cfg.QueueSize),
		done:          make(chan struct{}),
		batchSize:     cfg.BatchSize,
		flushInterval: cfg.FlushInterval,
	}
	go s.run()
	return s
}

func (c AsyncConfig) withDefaults() AsyncConfig {
	if c.QueueSize <= 0 {
		c.QueueSize = defaultAsyncQueueSize
	}
	if c.BatchSize <= 0 {
		c.BatchSize = defaultAsyncBatchSize
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = defaultAsyncFlushInterval
	}
	return c
}

func (s *AsyncSink) Emit(ctx context.Context, event *llmresult.Event) {
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

// run is the worker goroutine spawned by the constructor. It drains the
// bounded queue into the underlying sink in batches, flushing on either
// queue threshold or the idle flushInterval. Close signals the loop by
// closing the queue channel; the loop emits the remaining batch and
// returns.
func (s *AsyncSink) run() {
	defer close(s.done)
	batch := make([]*llmresult.Event, 0, s.batchSize)
	timer := time.NewTimer(s.flushInterval)
	stopTimer(timer)
	timerActive := false

	for {
		select {
		case event, ok := <-s.queue:
			if !ok {
				stopTimer(timer)
				s.emitBatch(batch)
				return
			}
			batch = append(batch, event)
			if len(batch) == 1 {
				timer.Reset(s.flushInterval)
				timerActive = true
			}
			if len(batch) >= s.batchSize {
				stopTimer(timer)
				timerActive = false
				s.emitBatch(batch)
				batch = batch[:0]
			}
		case <-timer.C:
			timerActive = false
			s.emitBatch(batch)
			batch = batch[:0]
		}

		if len(batch) == 0 && timerActive {
			stopTimer(timer)
			timerActive = false
		}
	}
}

func (s *AsyncSink) emitBatch(events []*llmresult.Event) {
	for _, event := range events {
		s.emitOne(event)
	}
}

func (s *AsyncSink) emitOne(event *llmresult.Event) {
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

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func (s *AsyncSink) recordDrop(ctx context.Context, event *llmresult.Event, reason string) {
	dropped := s.dropped.Add(1)
	s.log.LogAttrs(ctx, slog.LevelWarn, "llm result event dropped",
		slog.String("event_type", eventTypeOf(event)),
		slog.String("request_id", eventRequestID(event)),
		slog.String("reason", reason),
		slog.Uint64("dropped", dropped),
	)
}

func eventRequestID(event *llmresult.Event) string {
	if event == nil {
		return ""
	}
	return event.RequestID
}
