package telemetry

import (
	"context"
	"fmt"
	"log/slog"
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
type EventSink interface {
	Emit(ctx context.Context, event Event)
	Close() error
}

// NopSink drops every event.
type NopSink struct{}

func (NopSink) Emit(context.Context, Event) {}
func (NopSink) Close() error                { return nil }

type EventTypeFilterSink struct {
	next  EventSink
	allow map[string]struct{}
}

func NewEventTypeFilterSink(next EventSink, eventTypes ...string) EventSink {
	if next == nil {
		next = NopSink{}
	}
	allow := make(map[string]struct{}, len(eventTypes))
	for _, eventType := range eventTypes {
		if eventType != "" {
			allow[eventType] = struct{}{}
		}
	}
	return &EventTypeFilterSink{next: next, allow: allow}
}

func (s *EventTypeFilterSink) Emit(ctx context.Context, event Event) {
	if len(s.allow) == 0 {
		return
	}
	if _, ok := s.allow[eventTypeOf(event)]; !ok {
		return
	}
	s.next.Emit(ctx, event)
}

func (s *EventTypeFilterSink) Close() error {
	return s.next.Close()
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
		emitRecover(ctx, s.log, sink, event)
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

// RecoveringSink wraps one sink with panic isolation and structured reporting.
type RecoveringSink struct {
	next EventSink
	log  *slog.Logger
}

func NewRecoveringSink(next EventSink, log *slog.Logger) EventSink {
	if next == nil {
		next = NopSink{}
	}
	if log == nil {
		log = slog.Default()
	}
	return &RecoveringSink{next: next, log: log}
}

func (s *RecoveringSink) Emit(ctx context.Context, event Event) {
	emitRecover(ctx, s.log, s.next, event)
}

func (s *RecoveringSink) Close() error {
	return s.next.Close()
}

func emitRecover(ctx context.Context, log *slog.Logger, sink EventSink, event Event) {
	defer func() {
		if p := recover(); p != nil && log != nil {
			log.LogAttrs(ctx, slog.LevelError, "telemetry sink panic",
				slog.String("event_type", eventTypeOf(event)),
				slog.String("panic", fmt.Sprint(p)),
			)
		}
	}()
	sink.Emit(ctx, event)
}

func eventTypeOf(event Event) string {
	if event == nil {
		return ""
	}
	return event.TelemetryEventType()
}
