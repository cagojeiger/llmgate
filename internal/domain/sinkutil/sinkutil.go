// Package sinkutil defines a generic Sink contract plus shared
// implementations (Nop, Recovering) that any event-delivery pipeline
// in this codebase can specialize on its own event type. Two such
// pipelines exist today (telemetry events and llmresult events); both
// use these generic building blocks to avoid duplicating panic-recovery
// and no-op scaffolding per event type.
package sinkutil

import (
	"context"
	"log/slog"
)

// Sink delivers an event of type E to its underlying transport. Implementations
// must keep transport backpressure out of the request path — the per-pipeline
// caller is responsible for wrapping with a bounded async queue when remote.
type Sink[E any] interface {
	Emit(ctx context.Context, event E)
	Close() error
}

// Nop is the zero-effort Sink. Construct with `Nop[E]{}`.
type Nop[E any] struct{}

func (Nop[E]) Emit(context.Context, E) {}
func (Nop[E]) Close() error            { return nil }

// Recovering wraps next so an Emit panic is logged and isolated from
// the caller. label is the slog message; eventTypeOf extracts the
// event-type tag for the log record (each pipeline names its event
// type differently — telemetry vs llmresult — so the accessor is
// injected rather than constrained by a generic interface).
type Recovering[E any] struct {
	next        Sink[E]
	log         *slog.Logger
	label       string
	eventTypeOf func(E) string
}

func NewRecovering[E any](next Sink[E], log *slog.Logger, label string, eventTypeOf func(E) string) Sink[E] {
	if next == nil {
		next = Nop[E]{}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Recovering[E]{next: next, log: log, label: label, eventTypeOf: eventTypeOf}
}

func (s *Recovering[E]) Emit(ctx context.Context, event E) {
	defer func() {
		if p := recover(); p != nil {
			s.log.LogAttrs(ctx, slog.LevelError, s.label,
				slog.String("event_type", s.eventTypeOf(event)),
				slog.Any("panic", p),
			)
		}
	}()
	s.next.Emit(ctx, event)
}

func (s *Recovering[E]) Close() error {
	return s.next.Close()
}
