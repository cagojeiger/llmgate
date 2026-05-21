package sink

import (
	"context"
	"log/slog"
	"time"

	llmresult "llmgate/internal/domain/llmresult/schema"
)

func (s *AsyncSink) run() {
	defer close(s.done)
	batch := make([]*llmresult.Event, 0, s.batchSize)
	timer := time.NewTimer(s.flushInterval)
	stopTimer(timer)
	timerActive := false

	for {
		select {
		case event, ok := <-s.queue:
			if !ok {
				stopTimer(timer)
				s.emitBatch(batch)
				return
			}
			batch = append(batch, event)
			if len(batch) == 1 {
				timer.Reset(s.flushInterval)
				timerActive = true
			}
			if len(batch) >= s.batchSize {
				stopTimer(timer)
				timerActive = false
				s.emitBatch(batch)
				batch = batch[:0]
			}
		case <-timer.C:
			timerActive = false
			s.emitBatch(batch)
			batch = batch[:0]
		}

		if len(batch) == 0 && timerActive {
			stopTimer(timer)
			timerActive = false
		}
	}
}

func (s *AsyncSink) emitBatch(events []*llmresult.Event) {
	for _, event := range events {
		s.emitOne(event)
	}
}

func (s *AsyncSink) emitOne(event *llmresult.Event) {
	defer func() {
		if p := recover(); p != nil {
			s.log.LogAttrs(context.Background(), slog.LevelError, "llm result async sink panic",
				slog.String("event_type", eventTypeOf(event)),
				slog.Any("panic", p),
			)
		}
	}()
	s.next.Emit(context.Background(), event)
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}
