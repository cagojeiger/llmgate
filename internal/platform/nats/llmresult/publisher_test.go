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

// Apply nats.Option callbacks to a fresh Options struct and inspect the
// resulting User/Password fields — the same fields nats.Connect would
// read. Avoids needing a live broker to assert the auth wiring.
func applyOptions(t *testing.T, opts []natsgo.Option) natsgo.Options {
	t.Helper()
	var o natsgo.Options
	for _, opt := range opts {
		if err := opt(&o); err != nil {
			t.Fatalf("apply option: %v", err)
		}
	}
	return o
}

func TestConnectOptions_AnonymousWhenUserEmpty(t *testing.T) {
	got := applyOptions(t, connectOptions(Config{}))
	if got.User != "" || got.Password != "" {
		t.Fatalf("anonymous config attached creds: user=%q password=%q", got.User, got.Password)
	}
}

func TestConnectOptions_AttachesUserInfoWhenUserSet(t *testing.T) {
	got := applyOptions(t, connectOptions(Config{User: "llmgate", Password: "s3cret"}))
	if got.User != "llmgate" {
		t.Errorf("User = %q, want llmgate", got.User)
	}
	if got.Password != "s3cret" {
		t.Errorf("Password = %q, want s3cret", got.Password)
	}
}

func TestConnectOptions_IgnoresPasswordWithoutUser(t *testing.T) {
	// Password alone is meaningless to nats.UserInfo, so the option
	// should not be added (User stays empty → anonymous connect).
	got := applyOptions(t, connectOptions(Config{Password: "stray"}))
	if got.User != "" || got.Password != "" {
		t.Fatalf("password-only config attached creds: user=%q password=%q", got.User, got.Password)
	}
}
