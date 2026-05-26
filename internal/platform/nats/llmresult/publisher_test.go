package llmresult

import (
	"context"
	"encoding/json"
	"testing"

	natsgo "github.com/nats-io/nats.go"

	result "llmgate/internal/domain/llmresult/schema"
)

func TestPublisher_PublishWritesSubjectHeadersAndPayload(t *testing.T) {
	js := &fakeJetStream{}
	p := newPublisher(js, "results.finalized")

	err := p.Publish(context.Background(), &result.Event{
		SchemaVersion:  result.SchemaVersion,
		EventType:      result.EventType,
		RequestID:      "req-1",
		ModelRequested: "smart",
	})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if len(js.published) != 1 {
		t.Fatalf("published messages = %d, want 1", len(js.published))
	}
	got := js.published[0]
	if got.Subject != "results.finalized" {
		t.Fatalf("Subject = %q, want results.finalized", got.Subject)
	}
	var decoded result.Event
	if err := json.Unmarshal(got.Data, &decoded); err != nil {
		t.Fatalf("Data is not event JSON: %v", err)
	}
	if decoded.RequestID != "req-1" || decoded.ModelRequested != "smart" {
		t.Fatalf("decoded event = %+v", decoded)
	}
	if got.Header.Get(headerContentType) != contentTypeJSON {
		t.Fatalf("Content-Type = %q", got.Header.Get(headerContentType))
	}
	if got.Header.Get(headerEventType) != result.EventType {
		t.Fatalf("event header = %q", got.Header.Get(headerEventType))
	}
	if got.Header.Get(headerRequestID) != "req-1" {
		t.Fatalf("request header = %q", got.Header.Get(headerRequestID))
	}
	if got.Header.Get(headerSchemaVersion) != "1" {
		t.Fatalf("schema header = %q", got.Header.Get(headerSchemaVersion))
	}
}

func TestPublisher_RejectsNilEvent(t *testing.T) {
	p := newPublisher(&fakeJetStream{}, "results.finalized")

	if err := p.Publish(context.Background(), nil); err == nil {
		t.Fatal("Publish(nil) error = nil, want error")
	}
}

type fakeJetStream struct {
	published []*natsgo.Msg
}

func (f *fakeJetStream) PublishMsg(m *natsgo.Msg, _ ...natsgo.PubOpt) (*natsgo.PubAck, error) {
	f.published = append(f.published, m)
	return &natsgo.PubAck{}, nil
}
