package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultAsyncQueueSize    = 1024
	defaultAsyncWorkers      = 1
	defaultAsyncSendTimeout  = 5 * time.Second
	defaultAsyncFlushTimeout = 5 * time.Second
)

// AsyncEnqueuePolicy decides what Emit does when the bounded queue is full.
type AsyncEnqueuePolicy string

const (
	// AsyncDropNewest keeps the request path non-blocking by dropping the new
	// event when the queue is full.
	AsyncDropNewest AsyncEnqueuePolicy = "drop_newest"
	// AsyncBlockForTimeout lets Emit wait briefly for queue capacity before
	// dropping the event.
	AsyncBlockForTimeout AsyncEnqueuePolicy = "block_for_timeout"
)

// EventExporter is the remote delivery contract used behind AsyncSink. It is
// deliberately separate from EventSink because remote transports need an error
// channel, while request-path sinks must keep Emit fire-and-forget. Exporters
// should tolerate Close racing with an in-flight Export when shutdown flush
// times out.
type EventExporter interface {
	Export(ctx context.Context, event Event) error
	Close() error
}

// EventBatchExporter lets transports pipeline a group of events while keeping
// one broker message per event.
type EventBatchExporter interface {
	EventExporter
	ExportBatch(ctx context.Context, events []Event) error
}

// AsyncSinkObserver receives delivery-health facts from AsyncSink. Prometheus
// implements this interface; tests can use a lightweight capture.
type AsyncSinkObserver interface {
	AsyncEventEnqueued(sinkName, eventType string)
	AsyncEventDropped(sinkName, eventType, reason string)
	AsyncQueueDepth(sinkName string, depth int)
	AsyncSendError(sinkName, eventType string)
	AsyncFlushFinished(sinkName string, duration time.Duration)
}

type AsyncSinkConfig struct {
	Name           string
	QueueSize      int
	Workers        int
	BatchSize      int
	BatchMaxWait   time.Duration
	EnqueuePolicy  AsyncEnqueuePolicy
	EnqueueTimeout time.Duration
	SendTimeout    time.Duration
	FlushTimeout   time.Duration
	Observer       AsyncSinkObserver
	Log            *slog.Logger
}

type asyncItem struct {
	eventType string
	event     Event
}

// AsyncSink isolates remote telemetry delivery from the request path with a
// bounded in-memory queue. It is best-effort by design; stdout logs remain the
// durable evidence path for this stateless gateway.
type AsyncSink struct {
	name           string
	exporter       EventExporter
	queue          chan asyncItem
	enqueuePolicy  AsyncEnqueuePolicy
	enqueueTimeout time.Duration
	sendTimeout    time.Duration
	flushTimeout   time.Duration
	batchSize      int
	batchMaxWait   time.Duration
	observer       AsyncSinkObserver
	log            *slog.Logger
	mu             sync.RWMutex
	closed         atomic.Bool
	closeOnce      sync.Once
	closeCh        chan struct{}
	wg             sync.WaitGroup
	closeErr       error
}

func NewAsyncSink(exporter EventExporter, cfg AsyncSinkConfig) (*AsyncSink, error) {
	if exporter == nil {
		return nil, errors.New("telemetry async sink: exporter is nil")
	}
	name := cfg.Name
	if name == "" {
		name = "remote"
	}
	queueSize := cfg.QueueSize
	if queueSize <= 0 {
		queueSize = defaultAsyncQueueSize
	}
	workers := cfg.Workers
	if workers <= 0 {
		workers = defaultAsyncWorkers
	}
	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = 1
	}
	batchMaxWait := cfg.BatchMaxWait
	if batchSize > 1 && batchMaxWait <= 0 {
		batchMaxWait = time.Second
	}
	sendTimeout := cfg.SendTimeout
	if sendTimeout <= 0 {
		sendTimeout = defaultAsyncSendTimeout
	}
	flushTimeout := cfg.FlushTimeout
	if flushTimeout <= 0 {
		flushTimeout = defaultAsyncFlushTimeout
	}
	policy := cfg.EnqueuePolicy
	if policy == "" {
		policy = AsyncDropNewest
	}
	if policy != AsyncDropNewest && policy != AsyncBlockForTimeout {
		return nil, fmt.Errorf("telemetry async sink: unknown enqueue policy %q", policy)
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}

	s := &AsyncSink{
		name:           name,
		exporter:       exporter,
		queue:          make(chan asyncItem, queueSize),
		enqueuePolicy:  policy,
		enqueueTimeout: cfg.EnqueueTimeout,
		sendTimeout:    sendTimeout,
		flushTimeout:   flushTimeout,
		batchSize:      batchSize,
		batchMaxWait:   batchMaxWait,
		observer:       cfg.Observer,
		log:            log,
		closeCh:        make(chan struct{}),
	}
	for i := 0; i < workers; i++ {
		s.wg.Add(1)
		go s.run()
	}
	s.observeDepth()
	return s, nil
}

func (s *AsyncSink) Emit(_ context.Context, event Event) {
	if s == nil || event == nil {
		return
	}
	event = SnapshotEvent(event)
	item := asyncItem{eventType: eventTypeOf(event), event: event}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed.Load() {
		s.observeDrop(item.eventType, "closed")
		return
	}
	if s.enqueuePolicy == AsyncBlockForTimeout {
		s.emitBlockForTimeout(item)
		return
	}
	select {
	case s.queue <- item:
		s.observeEnqueue(item.eventType)
	case <-s.closeCh:
		s.observeDrop(item.eventType, "closed")
	default:
		s.observeDrop(item.eventType, "queue_full")
	}
}

func (s *AsyncSink) emitBlockForTimeout(item asyncItem) {
	if s.enqueueTimeout <= 0 {
		select {
		case s.queue <- item:
			s.observeEnqueue(item.eventType)
		case <-s.closeCh:
			s.observeDrop(item.eventType, "closed")
		default:
			s.observeDrop(item.eventType, "queue_full")
		}
		return
	}
	timer := time.NewTimer(s.enqueueTimeout)
	defer timer.Stop()
	select {
	case s.queue <- item:
		s.observeEnqueue(item.eventType)
	case <-s.closeCh:
		s.observeDrop(item.eventType, "closed")
	case <-timer.C:
		s.observeDrop(item.eventType, "queue_full")
	}
}

func (s *AsyncSink) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		start := time.Now()
		s.mu.Lock()
		s.closed.Store(true)
		close(s.closeCh)
		s.mu.Unlock()
		waitDone := make(chan struct{})
		go func() {
			s.wg.Wait()
			close(waitDone)
		}()
		timer := time.NewTimer(s.flushTimeout)
		defer timer.Stop()
		select {
		case <-waitDone:
		case <-timer.C:
			s.closeErr = errors.New("telemetry async sink: flush timeout")
		}
		if err := s.exporter.Close(); err != nil && s.closeErr == nil {
			s.closeErr = err
		}
		if s.closeErr != nil {
			s.log.Warn("telemetry async sink close failed",
				slog.String("sink", s.name),
				slog.String("err", s.closeErr.Error()),
			)
		}
		s.observeFlush(time.Since(start))
	})
	return s.closeErr
}

func (s *AsyncSink) run() {
	defer s.wg.Done()
	if s.batchSize > 1 {
		s.runBatch()
		return
	}
	for {
		select {
		case item := <-s.queue:
			s.export(item)
		case <-s.closeCh:
			for {
				select {
				case item := <-s.queue:
					s.export(item)
				default:
					return
				}
			}
		}
	}
}

func (s *AsyncSink) runBatch() {
	batch := make([]asyncItem, 0, s.batchSize)
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	var timerC <-chan time.Time
	startTimer := func() {
		if s.batchMaxWait <= 0 {
			return
		}
		timer.Reset(s.batchMaxWait)
		timerC = timer.C
	}
	stopTimer := func() {
		if timerC == nil {
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timerC = nil
	}
	flush := func() {
		if len(batch) == 0 {
			return
		}
		s.exportBatch(batch)
		batch = batch[:0]
		stopTimer()
	}
	drain := func() {
		for {
			select {
			case item := <-s.queue:
				batch = append(batch, item)
				if len(batch) >= s.batchSize {
					flush()
				}
			default:
				flush()
				return
			}
		}
	}
	defer timer.Stop()

	for {
		if len(batch) == 0 {
			select {
			case item := <-s.queue:
				batch = append(batch, item)
				startTimer()
				if len(batch) >= s.batchSize {
					flush()
				}
			case <-s.closeCh:
				drain()
				return
			}
			continue
		}
		select {
		case item := <-s.queue:
			batch = append(batch, item)
			if len(batch) >= s.batchSize {
				flush()
			}
		case <-timerC:
			timerC = nil
			flush()
		case <-s.closeCh:
			drain()
			return
		}
	}
}

func (s *AsyncSink) export(item asyncItem) {
	ctx, cancel := context.WithTimeout(context.Background(), s.sendTimeout)
	err := s.exporter.Export(ctx, item.event)
	cancel()
	if err != nil {
		s.observeSendError(item.eventType)
		s.log.Warn("telemetry async export failed",
			slog.String("sink", s.name),
			slog.String("event_type", item.eventType),
			slog.String("err", err.Error()),
		)
	}
	s.observeDepth()
}

func (s *AsyncSink) observeEnqueue(eventType string) {
	if s.observer != nil {
		s.observer.AsyncEventEnqueued(s.name, eventType)
	}
	s.observeDepth()
}

func (s *AsyncSink) observeDrop(eventType, reason string) {
	if s.observer != nil {
		s.observer.AsyncEventDropped(s.name, eventType, reason)
		s.observer.AsyncQueueDepth(s.name, len(s.queue))
	}
}

func (s *AsyncSink) exportBatch(items []asyncItem) {
	if len(items) == 0 {
		return
	}
	events := make([]Event, len(items))
	for i, item := range items {
		events[i] = item.event
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.sendTimeout)
	var err error
	if batcher, ok := s.exporter.(EventBatchExporter); ok {
		err = batcher.ExportBatch(ctx, events)
	} else {
		for _, event := range events {
			if exportErr := s.exporter.Export(ctx, event); exportErr != nil {
				err = errors.Join(err, exportErr)
			}
			if ctx.Err() != nil {
				err = errors.Join(err, ctx.Err())
				break
			}
		}
	}
	cancel()
	if err != nil {
		for _, item := range items {
			s.observeSendError(item.eventType)
		}
		s.log.Warn("telemetry async batch export failed",
			slog.String("sink", s.name),
			slog.Int("events", len(items)),
			slog.String("err", err.Error()),
		)
	}
	s.observeDepth()
}

func (s *AsyncSink) observeDepth() {
	if s.observer != nil {
		s.observer.AsyncQueueDepth(s.name, len(s.queue))
	}
}

func (s *AsyncSink) observeSendError(eventType string) {
	if s.observer != nil {
		s.observer.AsyncSendError(s.name, eventType)
	}
}

func (s *AsyncSink) observeFlush(d time.Duration) {
	if s.observer != nil {
		s.observer.AsyncFlushFinished(s.name, d)
	}
}
