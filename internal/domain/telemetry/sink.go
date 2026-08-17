package telemetry

import (
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

// FanoutSink fans each event out to every contained sink, isolating a
// panic in one so later sinks still receive the event. It is the shared
// sinkutil.Fanout specialized to telemetry's event type.
type FanoutSink = sinkutil.Fanout[Event]

func NewFanoutSink(log *slog.Logger, sinks ...EventSink) *FanoutSink {
	return sinkutil.NewFanout(log, "telemetry sink panic", eventTypeOf, sinks...)
}

func eventTypeOf(event Event) string {
	if event == nil {
		return ""
	}
	return event.TelemetryEventType()
}
