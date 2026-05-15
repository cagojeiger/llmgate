package telemetry

import "context"

// LifecycleObserver receives request/stream boundary notifications that are
// useful for live gauges such as in-flight requests and streams. Completed
// facts stay in AuditRecorder / CallRecorder.
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

// LifecycleObservers fans lifecycle notifications out to every observer.
type LifecycleObservers []LifecycleObserver

func (os LifecycleObservers) RequestStarted(ctx context.Context) {
	for _, o := range os {
		if o == nil {
			continue
		}
		o.RequestStarted(ctx)
	}
}

func (os LifecycleObservers) RequestFinished(ctx context.Context) {
	for _, o := range os {
		if o == nil {
			continue
		}
		o.RequestFinished(ctx)
	}
}

func (os LifecycleObservers) StreamStarted(ctx context.Context, common EventCommon) {
	for _, o := range os {
		if o == nil {
			continue
		}
		o.StreamStarted(ctx, common)
	}
}

func (os LifecycleObservers) StreamFinished(ctx context.Context, audit *AuditEvent, call *CallEvent) {
	for _, o := range os {
		if o == nil {
			continue
		}
		o.StreamFinished(ctx, audit, call)
	}
}
