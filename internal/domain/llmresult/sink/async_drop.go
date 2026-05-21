package sink

import (
	"context"
	"log/slog"

	llmresult "llmgate/internal/domain/llmresult/schema"
)

func (s *AsyncSink) recordDrop(ctx context.Context, event *llmresult.Event, reason string) {
	dropped := s.dropped.Add(1)
	s.log.LogAttrs(ctx, slog.LevelWarn, "llm result event dropped",
		slog.String("event_type", eventTypeOf(event)),
		slog.String("request_id", eventRequestID(event)),
		slog.String("reason", reason),
		slog.Uint64("dropped", dropped),
	)
}

func eventRequestID(event *llmresult.Event) string {
	if event == nil {
		return ""
	}
	return event.RequestID
}
