package telemetry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"llmgate/internal/llmtypes"
)

func TestAsyncSink_ExportsQueuedEventsOnClose(t *testing.T) {
	exporter := &captureExporter{}
	observer := &captureAsyncObserver{}
	sink, err := NewAsyncSink(exporter, AsyncSinkConfig{
		Name:      "stream",
		QueueSize: 4,
		Observer:  observer,
	})
	if err != nil {
		t.Fatalf("NewAsyncSink: %v", err)
	}

	sink.Emit(context.Background(), &AuditEvent{})
	sink.Emit(context.Background(), &CallEvent{})
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := exporter.count(); got != 2 {
		t.Fatalf("exported events = %d, want 2", got)
	}
	if got := observer.enqueued("stream", EventTypeAudit); got != 1 {
		t.Fatalf("audit enqueued = %d, want 1", got)
	}
	if got := observer.enqueued("stream", EventTypeCall); got != 1 {
		t.Fatalf("call enqueued = %d, want 1", got)
	}
	if observer.flushes == 0 {
		t.Fatalf("flush metric was not observed")
	}
}

func TestAsyncSink_DropsNewestWhenQueueFull(t *testing.T) {
	exporter := &blockingExporter{release: make(chan struct{})}
	observer := &captureAsyncObserver{}
	sink, err := NewAsyncSink(exporter, AsyncSinkConfig{
		Name:      "remote",
		QueueSize: 1,
		Observer:  observer,
	})
	if err != nil {
		t.Fatalf("NewAsyncSink: %v", err)
	}
	defer sink.Close()
	defer close(exporter.release)

	sink.Emit(context.Background(), &AuditEvent{})
	sink.Emit(context.Background(), &CallEvent{})
	sink.Emit(context.Background(), &CallEvent{})

	if got := observer.dropped("remote", EventTypeCall, "queue_full"); got == 0 {
		t.Fatalf("queue_full drops = %d, want at least 1", got)
	}
}

func TestAsyncSink_RecordsExporterErrors(t *testing.T) {
	exporter := &captureExporter{err: errors.New("broker unavailable")}
	observer := &captureAsyncObserver{}
	sink, err := NewAsyncSink(exporter, AsyncSinkConfig{
		Name:     "broker",
		Observer: observer,
	})
	if err != nil {
		t.Fatalf("NewAsyncSink: %v", err)
	}

	sink.Emit(context.Background(), &AuditEvent{})
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := observer.sendErrors("broker", EventTypeAudit); got != 1 {
		t.Fatalf("send errors = %d, want 1", got)
	}
}

func TestAsyncSink_BatchesBySize(t *testing.T) {
	exporter := &captureBatchExporter{}
	sink, err := NewAsyncSink(exporter, AsyncSinkConfig{
		Name:         "nats",
		QueueSize:    8,
		Workers:      1,
		BatchSize:    3,
		BatchMaxWait: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewAsyncSink: %v", err)
	}

	sink.Emit(context.Background(), &AuditEvent{})
	sink.Emit(context.Background(), &CallEvent{})
	sink.Emit(context.Background(), &CallEvent{})
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := exporter.batchSizes(); len(got) != 1 || got[0] != 3 {
		t.Fatalf("batch sizes = %v, want [3]", got)
	}
}

func TestAsyncSink_BatchesByMaxWait(t *testing.T) {
	exporter := &captureBatchExporter{}
	sink, err := NewAsyncSink(exporter, AsyncSinkConfig{
		Name:         "nats",
		QueueSize:    8,
		Workers:      1,
		BatchSize:    10,
		BatchMaxWait: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewAsyncSink: %v", err)
	}
	defer sink.Close()

	sink.Emit(context.Background(), &AuditEvent{})
	deadline := time.After(time.Second)
	for {
		if got := exporter.batchSizes(); len(got) == 1 && got[0] == 1 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("batch sizes = %v, want [1]", exporter.batchSizes())
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func TestAsyncSink_SnapshotsEventsBeforeExport(t *testing.T) {
	exporter := &blockingExporter{release: make(chan struct{})}
	sink, err := NewAsyncSink(exporter, AsyncSinkConfig{
		Name:      "remote",
		QueueSize: 2,
	})
	if err != nil {
		t.Fatalf("NewAsyncSink: %v", err)
	}

	call := &CallEvent{
		EventCommon: EventCommon{RequestID: "req-1"},
		Usage:       &llmtypes.Usage{PromptTokens: 1},
		Attempts: []llmtypes.Attempt{{
			Vendor: "first",
			Usage:  &llmtypes.Usage{CompletionTokens: 2},
		}},
	}
	sink.Emit(context.Background(), call)
	call.RequestID = "mutated"
	call.Usage.PromptTokens = 99
	call.Attempts[0].Vendor = "mutated"
	call.Attempts[0].Usage.CompletionTokens = 99

	close(exporter.release)
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, ok := exporter.first().(*CallEvent)
	if !ok {
		t.Fatalf("exported event type = %T, want *CallEvent", exporter.first())
	}
	if got.RequestID != "req-1" || got.Usage.PromptTokens != 1 || got.Attempts[0].Vendor != "first" || got.Attempts[0].Usage.CompletionTokens != 2 {
		t.Fatalf("event was not snapshotted before export: %+v", got)
	}
}

func TestAsyncSink_CloseTimesOut(t *testing.T) {
	exporter := &blockingExporter{release: make(chan struct{})}
	sink, err := NewAsyncSink(exporter, AsyncSinkConfig{
		FlushTimeout: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewAsyncSink: %v", err)
	}
	defer close(exporter.release)

	sink.Emit(context.Background(), &AuditEvent{})
	if err := sink.Close(); err == nil {
		t.Fatalf("Close error = nil, want flush timeout")
	}
}

type captureExporter struct {
	mu     sync.Mutex
	events []Event
	err    error
}

func (e *captureExporter) Export(_ context.Context, event Event) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, event)
	return e.err
}

func (e *captureExporter) Close() error { return nil }

func (e *captureExporter) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.events)
}

func (e *captureExporter) first() Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.events) == 0 {
		return nil
	}
	return e.events[0]
}

type captureBatchExporter struct {
	mu      sync.Mutex
	batches [][]Event
}

func (e *captureBatchExporter) Export(ctx context.Context, event Event) error {
	return e.ExportBatch(ctx, []Event{event})
}

func (e *captureBatchExporter) ExportBatch(_ context.Context, events []Event) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	copied := append([]Event(nil), events...)
	e.batches = append(e.batches, copied)
	return nil
}

func (e *captureBatchExporter) Close() error { return nil }

func (e *captureBatchExporter) batchSizes() []int {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]int, len(e.batches))
	for i, batch := range e.batches {
		out[i] = len(batch)
	}
	return out
}

type blockingExporter struct {
	mu      sync.Mutex
	events  []Event
	release chan struct{}
}

func (e *blockingExporter) Export(ctx context.Context, event Event) error {
	select {
	case <-e.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, event)
	return nil
}

func (e *blockingExporter) Close() error { return nil }

func (e *blockingExporter) first() Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.events) == 0 {
		return nil
	}
	return e.events[0]
}

type captureAsyncObserver struct {
	mu         sync.Mutex
	enqueues   map[[2]string]int
	drops      map[[3]string]int
	errors     map[[2]string]int
	depth      map[string]int
	flushes    int
	flushTotal time.Duration
}

func (o *captureAsyncObserver) AsyncEventEnqueued(sinkName, eventType string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.enqueues == nil {
		o.enqueues = make(map[[2]string]int)
	}
	o.enqueues[[2]string{sinkName, eventType}]++
}

func (o *captureAsyncObserver) AsyncEventDropped(sinkName, eventType, reason string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.drops == nil {
		o.drops = make(map[[3]string]int)
	}
	o.drops[[3]string{sinkName, eventType, reason}]++
}

func (o *captureAsyncObserver) AsyncQueueDepth(sinkName string, depth int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.depth == nil {
		o.depth = make(map[string]int)
	}
	o.depth[sinkName] = depth
}

func (o *captureAsyncObserver) AsyncSendError(sinkName, eventType string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.errors == nil {
		o.errors = make(map[[2]string]int)
	}
	o.errors[[2]string{sinkName, eventType}]++
}

func (o *captureAsyncObserver) AsyncFlushFinished(_ string, d time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.flushes++
	o.flushTotal += d
}

func (o *captureAsyncObserver) enqueued(sinkName, eventType string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.enqueues[[2]string{sinkName, eventType}]
}

func (o *captureAsyncObserver) dropped(sinkName, eventType, reason string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.drops[[3]string{sinkName, eventType, reason}]
}

func (o *captureAsyncObserver) sendErrors(sinkName, eventType string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.errors[[2]string{sinkName, eventType}]
}
