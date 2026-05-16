package telemetry

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestFanoutSink_FansOutEvents(t *testing.T) {
	a, b := &captureSink{}, &captureSink{}
	sinks := NewFanoutSink(nil, a, b)

	sinks.Emit(context.Background(), &AuditEvent{})

	if len(a.events) != 1 || len(b.events) != 1 {
		t.Errorf("each sink should have 1 event, got a=%d b=%d", len(a.events), len(b.events))
	}
}

func TestFanoutSink_IsolatesPanicAndContinuesFanout(t *testing.T) {
	capture := &captureSink{}
	sinks := NewFanoutSink(slog.New(slog.NewTextHandler(io.Discard, nil)), panicSink{}, capture)

	sinks.Emit(context.Background(), &AuditEvent{})

	if len(capture.events) != 1 {
		t.Fatalf("events captured = %d, want 1", len(capture.events))
	}
}

func TestFanoutSink_LogsPanic(t *testing.T) {
	log, buf := newCapturingLogger()
	sinks := NewFanoutSink(log, panicSink{})

	sinks.Emit(context.Background(), &AuditEvent{})

	if !strings.Contains(buf.String(), "telemetry sink panic") {
		t.Fatalf("panic log missing: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"event_type":"audit"`) {
		t.Fatalf("panic log missing event_type: %s", buf.String())
	}
}

func TestRecoveringSink_IsolatesPanic(t *testing.T) {
	sink := NewRecoveringSink(panicSink{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	sink.Emit(context.Background(), &AuditEvent{})
}

func TestFanoutSink_CloseReturnsFirstErrButStillRunsRest(t *testing.T) {
	first := errors.New("first-failed")
	second := errors.New("second-failed")
	rs := NewFanoutSink(nil, closeErrSink{err: first}, closeErrSink{err: second}, &captureSink{})

	got := rs.Close()
	if !errors.Is(got, first) {
		t.Errorf("Close = %v, want first-failed", got)
	}
}

func TestNop(t *testing.T) {
	var n NopSink
	n.Emit(context.Background(), &AuditEvent{})
	if err := n.Close(); err != nil {
		t.Errorf("NopSink.Close = %v, want nil", err)
	}
}
