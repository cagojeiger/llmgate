package telemetry

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

func TestLifecycleObservers(t *testing.T) {
	a, b := &captureLifecycleObserver{}, &captureLifecycleObserver{}
	os := LifecycleObservers{a, nil, b}

	os.RequestStarted(context.Background())
	os.RequestFinished(context.Background())
	os.StreamStarted(context.Background(), EventCommon{})
	os.StreamFinished(context.Background(), &AuditEvent{}, &CallEvent{})

	for name, got := range map[string]*captureLifecycleObserver{"a": a, "b": b} {
		if got.requestStarted != 1 || got.requestFinished != 1 || got.streamStarted != 1 || got.streamFinished != 1 {
			t.Errorf("%s lifecycle calls = %+v, want all 1", name, got)
		}
	}
}

func TestRecoveringLifecycleObserver_IsolatesPanic(t *testing.T) {
	observer := NewRecoveringLifecycleObserver(panicLifecycleObserver{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	observer.RequestStarted(context.Background())
	observer.RequestFinished(context.Background())
	observer.StreamStarted(context.Background(), EventCommon{})
	observer.StreamFinished(context.Background(), &AuditEvent{}, &CallEvent{})
}
