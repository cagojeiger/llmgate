package transport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"llmgate/internal/events/llmresult"
)

func TestJSONEncoder_EncodesEventMetadataAndPayload(t *testing.T) {
	event := &llmresult.Event{
		SchemaVersion:  llmresult.SchemaVersion,
		EventType:      llmresult.EventType,
		RequestID:      "req-1",
		ModelRequested: "smart",
	}

	got, err := (JSONEncoder{}).Encode(event)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	if got.ContentType != ContentTypeJSON {
		t.Fatalf("ContentType = %q, want %q", got.ContentType, ContentTypeJSON)
	}
	if got.EventType != llmresult.EventType || got.RequestID != "req-1" || got.SchemaVersion != llmresult.SchemaVersion {
		t.Fatalf("message metadata = %+v", got)
	}
	var decoded llmresult.Event
	if err := json.Unmarshal(got.Payload, &decoded); err != nil {
		t.Fatalf("payload is not event JSON: %v", err)
	}
	if decoded.ModelRequested != "smart" {
		t.Fatalf("decoded ModelRequested = %q, want smart", decoded.ModelRequested)
	}
}

func TestJSONEncoder_RejectsNilEvent(t *testing.T) {
	if _, err := (JSONEncoder{}).Encode(nil); err == nil {
		t.Fatal("Encode(nil) error = nil, want error")
	}
}

func TestSink_PublishesEncodedMessage(t *testing.T) {
	publisher := &capturePublisher{}
	sink := NewSink(publisher, JSONEncoder{}, discardLogger())

	sink.Emit(context.Background(), &llmresult.Event{
		SchemaVersion: llmresult.SchemaVersion,
		EventType:     llmresult.EventType,
		RequestID:     "req-1",
	})

	if len(publisher.messages) != 1 {
		t.Fatalf("published messages = %d, want 1", len(publisher.messages))
	}
	if publisher.messages[0].RequestID != "req-1" {
		t.Fatalf("message RequestID = %q, want req-1", publisher.messages[0].RequestID)
	}
}

func TestSink_SwallowsPublishError(t *testing.T) {
	sink := NewSink(&errorPublisher{err: errors.New("publish failed")}, JSONEncoder{}, discardLogger())

	sink.Emit(context.Background(), &llmresult.Event{
		SchemaVersion: llmresult.SchemaVersion,
		EventType:     llmresult.EventType,
		RequestID:     "req-1",
	})
}

func TestSink_CloseClosesPublisher(t *testing.T) {
	publisher := &capturePublisher{}
	sink := NewSink(publisher, JSONEncoder{}, discardLogger())

	if err := sink.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if !publisher.closed {
		t.Fatal("publisher was not closed")
	}
}

type capturePublisher struct {
	messages []Message
	closed   bool
}

func (p *capturePublisher) Publish(_ context.Context, msg Message) error {
	p.messages = append(p.messages, msg)
	return nil
}

func (p *capturePublisher) Close() error {
	p.closed = true
	return nil
}

type errorPublisher struct {
	err error
}

func (p *errorPublisher) Publish(context.Context, Message) error { return p.err }
func (p *errorPublisher) Close() error                           { return nil }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
