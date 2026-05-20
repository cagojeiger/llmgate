package nats

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"llmgate/internal/events/llmresult/transport"
)

func TestPublisher_IntegrationJetStream(t *testing.T) {
	url := os.Getenv("LLMGATE_TEST_NATS_URL")
	if url == "" {
		t.Skip("set LLMGATE_TEST_NATS_URL to run NATS integration test")
	}

	stream := "LLMRESULT_TEST_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	subject := "llmgate.test." + stream
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	publisher, err := NewPublisher(ctx, Config{
		URL:     url,
		Stream:  stream,
		Subject: subject,
	})
	if err != nil {
		t.Fatalf("NewPublisher() error = %v", err)
	}
	defer publisher.Close()

	nc, err := natsgo.Connect(url)
	if err != nil {
		t.Fatalf("connect for verification: %v", err)
	}
	defer nc.Close()
	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("verification JetStream: %v", err)
	}
	defer js.DeleteStream(stream)

	payload := []byte(`{"event_type":"llm.result.finalized","request_id":"req-it"}`)
	if err := publisher.Publish(ctx, transport.Message{
		ContentType:   transport.ContentTypeJSON,
		EventType:     "llm.result.finalized",
		RequestID:     "req-it",
		SchemaVersion: 1,
		Payload:       payload,
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	got, err := js.GetLastMsg(stream, subject, natsgo.Context(ctx))
	if err != nil {
		t.Fatalf("GetLastMsg() error = %v", err)
	}
	if string(got.Data) != string(payload) {
		t.Fatalf("stored payload = %s, want %s", string(got.Data), string(payload))
	}
}
