package llmresult

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	result "llmgate/internal/domain/llmresult/schema"
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

	nc, err := natsgo.Connect(url)
	if err != nil {
		t.Fatalf("connect for setup/verification: %v", err)
	}
	defer nc.Close()
	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("setup JetStream: %v", err)
	}
	if _, err := js.AddStream(&natsgo.StreamConfig{
		Name:      stream,
		Subjects:  []string{subject},
		Retention: natsgo.LimitsPolicy,
		Storage:   natsgo.FileStorage,
		Discard:   natsgo.DiscardOld,
		MaxBytes:  1024 * 1024,
	}, natsgo.Context(ctx)); err != nil {
		t.Fatalf("AddStream() error = %v", err)
	}
	defer js.DeleteStream(stream)

	publisher, err := NewPublisher(ctx, Config{
		URL:     url,
		Subject: subject,
	}, nil)
	if err != nil {
		t.Fatalf("NewPublisher() error = %v", err)
	}
	defer publisher.Close()

	if err := publisher.Publish(ctx, &result.Event{
		SchemaVersion: result.SchemaVersion,
		EventType:     result.EventType,
		RequestID:     "req-it",
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	got, err := js.GetLastMsg(stream, subject, natsgo.Context(ctx))
	if err != nil {
		t.Fatalf("GetLastMsg() error = %v", err)
	}
	var decoded result.Event
	if err := json.Unmarshal(got.Data, &decoded); err != nil {
		t.Fatalf("stored payload is not event JSON: %v", err)
	}
	if decoded.RequestID != "req-it" || decoded.EventType != result.EventType {
		t.Fatalf("stored event = %+v", decoded)
	}
}
