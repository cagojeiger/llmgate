package sink

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	llmresult "llmgate/internal/domain/llmresult/schema"
)

const (
	defaultAsyncQueueSize     = 1000
	defaultAsyncBatchSize     = 100
	defaultAsyncFlushInterval = time.Second
	// defaultAsyncEmitTimeout bounds one downstream Emit (e.g. NATS
	// Publish) so a stuck broker cannot freeze the drain loop.
	defaultAsyncEmitTimeout = 10 * time.Second
	// defaultAsyncCloseTimeout bounds Close() waiting on the worker.
	// It fits inside the post-drain headroom operators are expected to
	// leave under ShutdownDrainTimeout — k8s
	// terminationGracePeriodSeconds should be ≥ drain + close + α.
	defaultAsyncCloseTimeout = 60 * time.Second
)

type AsyncConfig struct {
	QueueSize     int
	BatchSize     int
	FlushInterval time.Duration
	// EmitTimeout caps one downstream Emit call. 0 → default.
	EmitTimeout time.Duration
	// CloseTimeout caps Close()'s wait on the worker goroutine. 0 → default.
	CloseTimeout time.Duration
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
	emitTimeout   time.Duration
	closeTimeout  time.Duration

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
		emitTimeout:   cfg.EmitTimeout,
		closeTimeout:  cfg.CloseTimeout,
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
	if c.EmitTimeout == 0 {
		c.EmitTimeout = defaultAsyncEmitTimeout
	}
	if c.CloseTimeout == 0 {
		c.CloseTimeout = defaultAsyncCloseTimeout
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

		start := time.Now()
		select {
		case <-s.done:
			s.log.LogAttrs(context.Background(), slog.LevelInfo,
				"llm result async sink drained",
				slog.Int64("duration_ms", time.Since(start).Milliseconds()),
				slog.Uint64("dropped_total", s.dropped.Load()),
			)
		case <-time.After(s.closeTimeout):
			// Worker is still inside next.Emit (broker hang). We
			// abandon it: the underlying NATS conn close below will
			// usually unblock it; if not, the worker goroutine
			// outlives Close until process exit. queue_remaining is
			// how many already-enqueued events the worker had not yet
			// handed to next.Emit at the abandon point — those are
			// lost.
			s.log.LogAttrs(context.Background(), slog.LevelWarn,
				"llm result async sink close timeout — worker abandoned",
				slog.Duration("budget", s.closeTimeout),
				slog.Int("queue_remaining", len(s.queue)),
				slog.Uint64("dropped_total", s.dropped.Load()),
			)
		}
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
	defer func() {
		if p := recover(); p != nil {
			s.log.LogAttrs(context.Background(), slog.LevelError,
				"async sink worker panic",
				slog.Any("panic", p),
				slog.String("stack", string(debug.Stack())),
			)
		}
	}()
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
	// AsyncSink is a background worker; downstream Emit must outlive any
	// individual request ctx, so we deliberately detach from caller ctx.
	ctx, cancel := context.WithTimeout(context.Background(), s.emitTimeout) //nolint:contextcheck // intentional detach from request ctx
	defer cancel()
	s.next.Emit(ctx, event)
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
