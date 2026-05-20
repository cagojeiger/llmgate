package llmresult

import (
	"context"
	"log/slog"
)

// Sink receives finalized LLM result events. Implementations must keep
// transport backpressure out of the request path.
type Sink interface {
	Emit(ctx context.Context, event *Event)
	Close() error
}

// NopSink drops finalized result events.
type NopSink struct{}

func (NopSink) Emit(context.Context, *Event) {}
func (NopSink) Close() error                 { return nil }

// RecoveringSink isolates sink panics from the HTTP request path.
type RecoveringSink struct {
	next Sink
	log  *slog.Logger
}

func NewRecoveringSink(next Sink, log *slog.Logger) Sink {
	if next == nil {
		next = NopSink{}
	}
	if log == nil {
		log = slog.Default()
	}
	return &RecoveringSink{next: next, log: log}
}

func (s *RecoveringSink) Emit(ctx context.Context, event *Event) {
	defer func() {
		if p := recover(); p != nil {
			s.log.LogAttrs(ctx, slog.LevelError, "llm result sink panic",
				slog.String("event_type", eventTypeOf(event)),
				slog.Any("panic", p),
			)
		}
	}()
	s.next.Emit(ctx, event)
}

func (s *RecoveringSink) Close() error {
	return s.next.Close()
}

func eventTypeOf(event *Event) string {
	if event == nil {
		return ""
	}
	return event.AnalyticsEventType()
}
