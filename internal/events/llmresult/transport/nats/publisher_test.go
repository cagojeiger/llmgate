package nats

import (
	"context"
	"errors"
	"testing"

	natsgo "github.com/nats-io/nats.go"

	"llmgate/internal/events/llmresult/transport"
)

func TestPublisher_PublishWritesSubjectHeadersAndPayload(t *testing.T) {
	js := &fakeJetStream{}
	p := newPublisher(js, "RESULTS", "results.finalized")

	err := p.Publish(context.Background(), transport.Message{
		ContentType:   transport.ContentTypeJSON,
		EventType:     "llm.result.finalized",
		RequestID:     "req-1",
		SchemaVersion: 1,
		Payload:       []byte(`{"ok":true}`),
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
	if string(got.Data) != `{"ok":true}` {
		t.Fatalf("Data = %q", string(got.Data))
	}
	if got.Header.Get(headerContentType) != transport.ContentTypeJSON {
		t.Fatalf("Content-Type = %q", got.Header.Get(headerContentType))
	}
	if got.Header.Get(headerEventType) != "llm.result.finalized" {
		t.Fatalf("event header = %q", got.Header.Get(headerEventType))
	}
	if got.Header.Get(headerRequestID) != "req-1" {
		t.Fatalf("request header = %q", got.Header.Get(headerRequestID))
	}
	if got.Header.Get(headerSchemaVersion) != "1" {
		t.Fatalf("schema header = %q", got.Header.Get(headerSchemaVersion))
	}
}

func TestPublisher_EnsureStreamCreatesMissingStream(t *testing.T) {
	js := &fakeJetStream{streamInfoErr: natsgo.ErrStreamNotFound}
	p := newPublisher(js, "RESULTS", "results.finalized")

	if err := p.ensureStream(context.Background()); err != nil {
		t.Fatalf("ensureStream() error = %v", err)
	}
	if js.added == nil {
		t.Fatal("AddStream was not called")
	}
	if js.added.Name != "RESULTS" {
		t.Fatalf("stream name = %q, want RESULTS", js.added.Name)
	}
	if len(js.added.Subjects) != 1 || js.added.Subjects[0] != "results.finalized" {
		t.Fatalf("subjects = %v", js.added.Subjects)
	}
}

func TestPublisher_EnsureStreamKeepsExistingStream(t *testing.T) {
	js := &fakeJetStream{}
	p := newPublisher(js, "RESULTS", "results.finalized")

	if err := p.ensureStream(context.Background()); err != nil {
		t.Fatalf("ensureStream() error = %v", err)
	}
	if js.added != nil {
		t.Fatalf("AddStream called for existing stream: %+v", js.added)
	}
}

func TestPublisher_EnsureStreamReturnsInspectError(t *testing.T) {
	want := errors.New("inspect failed")
	js := &fakeJetStream{streamInfoErr: want}
	p := newPublisher(js, "RESULTS", "results.finalized")

	if err := p.ensureStream(context.Background()); !errors.Is(err, want) {
		t.Fatalf("ensureStream() error = %v, want %v", err, want)
	}
}

type fakeJetStream struct {
	published     []*natsgo.Msg
	streamInfoErr error
	addErr        error
	added         *natsgo.StreamConfig
}

func (f *fakeJetStream) PublishMsg(m *natsgo.Msg, _ ...natsgo.PubOpt) (*natsgo.PubAck, error) {
	f.published = append(f.published, m)
	return &natsgo.PubAck{}, nil
}

func (f *fakeJetStream) StreamInfo(stream string, _ ...natsgo.JSOpt) (*natsgo.StreamInfo, error) {
	if f.streamInfoErr != nil {
		return nil, f.streamInfoErr
	}
	return &natsgo.StreamInfo{Config: natsgo.StreamConfig{Name: stream}}, nil
}

func (f *fakeJetStream) AddStream(cfg *natsgo.StreamConfig, _ ...natsgo.JSOpt) (*natsgo.StreamInfo, error) {
	if f.addErr != nil {
		return nil, f.addErr
	}
	f.added = cfg
	return &natsgo.StreamInfo{Config: *cfg}, nil
}
