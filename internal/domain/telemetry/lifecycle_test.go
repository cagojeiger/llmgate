package telemetry

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

func TestLifecycleObservers(t *testing.T) {
	a, b := &captureLifecycleObserver{}, &captureLifecycleObserver{}
	os := NewLifecycleObservers(slog.New(slog.NewTextHandler(io.Discard, nil)), a, nil, b)

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

func TestLifecycleObservers_IsolatesPanic(t *testing.T) {
	capture := &captureLifecycleObserver{}
	observer := NewLifecycleObservers(slog.New(slog.NewTextHandler(io.Discard, nil)), panicLifecycleObserver{}, capture)

	observer.RequestStarted(context.Background())
	observer.RequestFinished(context.Background())
	observer.StreamStarted(context.Background(), EventCommon{})
	observer.StreamFinished(context.Background(), &AuditEvent{}, &CallEvent{})

	if capture.requestStarted != 1 || capture.requestFinished != 1 || capture.streamStarted != 1 || capture.streamFinished != 1 {
		t.Fatalf("capture lifecycle calls = %+v, want all 1", capture)
	}
}
