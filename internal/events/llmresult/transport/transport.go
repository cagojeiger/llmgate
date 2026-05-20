package transport

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"llmgate/internal/events/llmresult"
)

const ContentTypeJSON = "application/json"

type Message struct {
	ContentType   string
	EventType     string
	RequestID     string
	SchemaVersion int
	Payload       []byte
}

type Publisher interface {
	Publish(ctx context.Context, msg Message) error
	Close() error
}

type Encoder interface {
	Encode(event *llmresult.Event) (Message, error)
}

type JSONEncoder struct{}

func (JSONEncoder) Encode(event *llmresult.Event) (Message, error) {
	if event == nil {
		return Message{}, errors.New("llm result event is nil")
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return Message{}, err
	}
	return Message{
		ContentType:   ContentTypeJSON,
		EventType:     event.EventType,
		RequestID:     event.RequestID,
		SchemaVersion: event.SchemaVersion,
		Payload:       payload,
	}, nil
}

type Sink struct {
	publisher Publisher
	encoder   Encoder
	log       *slog.Logger
}

func NewSink(publisher Publisher, encoder Encoder, log *slog.Logger) *Sink {
	if encoder == nil {
		encoder = JSONEncoder{}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Sink{publisher: publisher, encoder: encoder, log: log}
}

func (s *Sink) Emit(ctx context.Context, event *llmresult.Event) {
	if s == nil || s.publisher == nil || event == nil {
		return
	}
	msg, err := s.encoder.Encode(event)
	if err != nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "encode llm result event failed",
			slog.String("event_type", eventTypeOf(event)),
			slog.String("request_id", requestIDOf(event)),
			slog.String("err", err.Error()),
		)
		return
	}
	if err := s.publisher.Publish(ctx, msg); err != nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "publish llm result event failed",
			slog.String("event_type", msg.EventType),
			slog.String("request_id", msg.RequestID),
			slog.String("err", err.Error()),
		)
	}
}

func (s *Sink) Close() error {
	if s == nil || s.publisher == nil {
		return nil
	}
	return s.publisher.Close()
}

func eventTypeOf(event *llmresult.Event) string {
	if event == nil {
		return ""
	}
	return event.AnalyticsEventType()
}

func requestIDOf(event *llmresult.Event) string {
	if event == nil {
		return ""
	}
	return event.RequestID
}
