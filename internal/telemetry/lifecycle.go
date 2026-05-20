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

// LifecycleObservers fans lifecycle notifications out to every observer and
// isolates observer panics from the request path.
type LifecycleObservers struct {
	log       *slog.Logger
	observers []LifecycleObserver
}

func NewLifecycleObservers(log *slog.Logger, observers ...LifecycleObserver) LifecycleObserver {
	if log == nil {
		log = slog.Default()
	}
	return &LifecycleObservers{log: log, observers: observers}
}

func (os *LifecycleObservers) RequestStarted(ctx context.Context) {
	for _, o := range os.observers {
		if o == nil {
			continue
		}
		os.observe(ctx, "request_started", func() { o.RequestStarted(ctx) })
	}
}

func (os *LifecycleObservers) RequestFinished(ctx context.Context) {
	for _, o := range os.observers {
		if o == nil {
			continue
		}
		os.observe(ctx, "request_finished", func() { o.RequestFinished(ctx) })
	}
}

func (os *LifecycleObservers) StreamStarted(ctx context.Context, common EventCommon) {
	for _, o := range os.observers {
		if o == nil {
			continue
		}
		os.observe(ctx, "stream_started", func() { o.StreamStarted(ctx, common) })
	}
}

func (os *LifecycleObservers) StreamFinished(ctx context.Context, audit *AuditEvent, call *CallEvent) {
	for _, o := range os.observers {
		if o == nil {
			continue
		}
		os.observe(ctx, "stream_finished", func() { o.StreamFinished(ctx, audit, call) })
	}
}

func (os *LifecycleObservers) observe(ctx context.Context, hook string, fn func()) {
	defer func() {
		if p := recover(); p != nil {
			os.log.LogAttrs(ctx, slog.LevelError, "telemetry lifecycle panic",
				slog.String("hook", hook),
				slog.String("panic", fmt.Sprint(p)),
			)
		}
	}()
	fn()
}
