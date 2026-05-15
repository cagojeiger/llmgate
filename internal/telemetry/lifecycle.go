package telemetry

import (
	"context"
	"fmt"
	"log/slog"
)

// LifecycleObserver receives request/stream boundary notifications that are
// useful for live gauges such as in-flight requests and streams. Completed
// facts stay in AuditEvent / CallEvent delivery through EventSink.
type LifecycleObserver interface {
	RequestStarted(ctx context.Context)
	RequestFinished(ctx context.Context)
	StreamStarted(ctx context.Context, common EventCommon)
	StreamFinished(ctx context.Context, audit *AuditEvent, call *CallEvent)
}

// NopLifecycleObserver drops lifecycle notifications.
type NopLifecycleObserver struct{}

func (NopLifecycleObserver) RequestStarted(context.Context)                          {}
func (NopLifecycleObserver) RequestFinished(context.Context)                         {}
func (NopLifecycleObserver) StreamStarted(context.Context, EventCommon)              {}
func (NopLifecycleObserver) StreamFinished(context.Context, *AuditEvent, *CallEvent) {}

// RecoveringLifecycleObserver isolates observer panics from the request path.
type RecoveringLifecycleObserver struct {
	next LifecycleObserver
	log  *slog.Logger
}

func NewRecoveringLifecycleObserver(next LifecycleObserver, log *slog.Logger) LifecycleObserver {
	if next == nil {
		next = NopLifecycleObserver{}
	}
	if log == nil {
		log = slog.Default()
	}
	return &RecoveringLifecycleObserver{next: next, log: log}
}

func (o *RecoveringLifecycleObserver) RequestStarted(ctx context.Context) {
	o.observe(ctx, "request_started", func() { o.next.RequestStarted(ctx) })
}

func (o *RecoveringLifecycleObserver) RequestFinished(ctx context.Context) {
	o.observe(ctx, "request_finished", func() { o.next.RequestFinished(ctx) })
}

func (o *RecoveringLifecycleObserver) StreamStarted(ctx context.Context, common EventCommon) {
	o.observe(ctx, "stream_started", func() { o.next.StreamStarted(ctx, common) })
}

func (o *RecoveringLifecycleObserver) StreamFinished(ctx context.Context, audit *AuditEvent, call *CallEvent) {
	o.observe(ctx, "stream_finished", func() { o.next.StreamFinished(ctx, audit, call) })
}

func (o *RecoveringLifecycleObserver) observe(ctx context.Context, hook string, fn func()) {
	defer func() {
		if p := recover(); p != nil {
			o.log.LogAttrs(ctx, slog.LevelError, "telemetry lifecycle panic",
				slog.String("hook", hook),
				slog.String("panic", fmt.Sprint(p)),
			)
		}
	}()
	fn()
}

// LifecycleObservers fans lifecycle notifications out to every observer.
type LifecycleObservers []LifecycleObserver

func (os LifecycleObservers) RequestStarted(ctx context.Context) {
	for _, o := range os {
		if o == nil {
			continue
		}
		observeRecover(ctx, nil, "request_started", func() { o.RequestStarted(ctx) })
	}
}

func (os LifecycleObservers) RequestFinished(ctx context.Context) {
	for _, o := range os {
		if o == nil {
			continue
		}
		observeRecover(ctx, nil, "request_finished", func() { o.RequestFinished(ctx) })
	}
}

func (os LifecycleObservers) StreamStarted(ctx context.Context, common EventCommon) {
	for _, o := range os {
		if o == nil {
			continue
		}
		observeRecover(ctx, nil, "stream_started", func() { o.StreamStarted(ctx, common) })
	}
}

func (os LifecycleObservers) StreamFinished(ctx context.Context, audit *AuditEvent, call *CallEvent) {
	for _, o := range os {
		if o == nil {
			continue
		}
		observeRecover(ctx, nil, "stream_finished", func() { o.StreamFinished(ctx, audit, call) })
	}
}

func observeRecover(ctx context.Context, log *slog.Logger, hook string, fn func()) {
	defer func() {
		if p := recover(); p != nil && log != nil {
			log.LogAttrs(ctx, slog.LevelError, "telemetry lifecycle panic",
				slog.String("hook", hook),
				slog.String("panic", fmt.Sprint(p)),
			)
		}
	}()
	fn()
}
