package sink

import (
	"context"
	"log/slog"

	llmresultschema "llmgate/internal/domain/llmresult/schema"
)

// FanoutSink delivers each finalized event to every contained sink. It
// lets the audit (local-rotate + S3) and NATS sinks run side by side so
// existing NATS consumers keep receiving events while the audit path is
// rolled out. A panic in one sink is logged and isolated so the others
// still receive the event.
type FanoutSink struct {
	log   *slog.Logger
	sinks []Sink
}

func NewFanoutSink(log *slog.Logger, sinks ...Sink) *FanoutSink {
	if log == nil {
		log = slog.Default()
	}
	return &FanoutSink{log: log, sinks: sinks}
}

func (s *FanoutSink) Emit(ctx context.Context, event *llmresultschema.Event) {
	for _, next := range s.sinks {
		if next == nil {
			continue
		}
		// Wrap per-call so one sink's panic does not stop the others.
		NewRecoveringSink(next, s.log).Emit(ctx, event)
	}
}

func (s *FanoutSink) Close() error {
	var firstErr error
	for _, next := range s.sinks {
		if next == nil {
			continue
		}
		if err := next.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
