package telemetry

import (
	"context"
	"log/slog"

	"llmgate/internal/domain/sinkutil"
)

// Event is the common contract for telemetry facts emitted by the gateway.
// Concrete event structs keep domain-specific fields; the sink boundary only
// needs the stable event type so delivery can be fanned out safely.
type Event interface {
	TelemetryEventType() string
}

// EventSink receives finalized telemetry events. Implementations must not
// assume they can block the request path indefinitely; future remote sinks
// should wrap their transport with a bounded async queue.
type EventSink = sinkutil.Sink[Event]

// NopSink drops every event.
type NopSink = sinkutil.Nop[Event]

// NewRecoveringSink wraps next so a panic in Emit is logged and isolated.
func NewRecoveringSink(next EventSink, log *slog.Logger) EventSink {
	return sinkutil.NewRecovering(next, log, "telemetry sink panic", eventTypeOf)
}

// FanoutSink fans each event out to every contained sink. A panic in one sink
// is logged and isolated so later sinks still receive the event.
type FanoutSink struct {
	log   *slog.Logger
	sinks []EventSink
}

func NewFanoutSink(log *slog.Logger, sinks ...EventSink) *FanoutSink {
	if log == nil {
		log = slog.Default()
	}
	return &FanoutSink{log: log, sinks: sinks}
}

func (s *FanoutSink) Emit(ctx context.Context, event Event) {
	for _, sink := range s.sinks {
		if sink == nil {
			continue
		}
		// Wrap per-call so one sink panic does not stop later sinks.
		sinkutil.NewRecovering(sink, s.log, "telemetry sink panic", eventTypeOf).Emit(ctx, event)
	}
}

func (s *FanoutSink) Close() error {
	var firstErr error
	for _, sink := range s.sinks {
		if sink == nil {
			continue
		}
		if err := sink.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func eventTypeOf(event Event) string {
	if event == nil {
		return ""
	}
	return event.TelemetryEventType()
}
