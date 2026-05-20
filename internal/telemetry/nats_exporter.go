package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

type NATSExporterConfig struct {
	URL     string
	Subject string
	Stream  string
	Name    string
}

type NATSJetStreamExporter struct {
	nc      *nats.Conn
	js      jetstream.JetStream
	subject string
}

func NewNATSJetStreamExporter(cfg NATSExporterConfig) (*NATSJetStreamExporter, error) {
	if cfg.URL == "" {
		return nil, errors.New("nats exporter: URL is required")
	}
	subject := cfg.Subject
	if subject == "" {
		subject = LLMCallFinalizedSubject
	}
	name := cfg.Name
	if name == "" {
		name = "llmgate"
	}
	nc, err := nats.Connect(cfg.URL, nats.Name(name))
	if err != nil {
		return nil, err
	}
	js, err := jetstream.New(nc, jetstream.WithPublishAsyncMaxPending(1024))
	if err != nil {
		nc.Close()
		return nil, err
	}
	if cfg.Stream != "" {
		_, err = js.CreateOrUpdateStream(context.Background(), jetstream.StreamConfig{
			Name:      cfg.Stream,
			Subjects:  []string{subject},
			Retention: jetstream.LimitsPolicy,
			Discard:   jetstream.DiscardOld,
			Storage:   jetstream.FileStorage,
			Replicas:  1,
		})
		if err != nil {
			nc.Close()
			return nil, err
		}
	}
	return &NATSJetStreamExporter{nc: nc, js: js, subject: subject}, nil
}

func (e *NATSJetStreamExporter) Export(ctx context.Context, event Event) error {
	if e == nil || event == nil {
		return nil
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	opts := publishOptsForEvent(event)
	_, err = e.js.Publish(ctx, e.subject, payload, opts...)
	return err
}

func (e *NATSJetStreamExporter) ExportBatch(ctx context.Context, events []Event) error {
	if e == nil || len(events) == 0 {
		return nil
	}
	futures := make([]jetstream.PubAckFuture, 0, len(events))
	var joined error
	for _, event := range events {
		if event == nil {
			continue
		}
		payload, err := json.Marshal(event)
		if err != nil {
			joined = errors.Join(joined, err)
			continue
		}
		future, err := e.js.PublishAsync(e.subject, payload, publishOptsForEvent(event)...)
		if err != nil {
			joined = errors.Join(joined, err)
			continue
		}
		futures = append(futures, future)
	}
	for _, future := range futures {
		select {
		case <-future.Ok():
		case err := <-future.Err():
			if err != nil {
				joined = errors.Join(joined, err)
			} else {
				joined = errors.Join(joined, fmt.Errorf("nats async publish failed without error"))
			}
		case <-ctx.Done():
			return errors.Join(joined, ctx.Err())
		}
	}
	return joined
}

func (e *NATSJetStreamExporter) Close() error {
	if e == nil || e.nc == nil {
		return nil
	}
	e.nc.Drain()
	e.nc.Close()
	return nil
}

func publishOptsForEvent(event Event) []jetstream.PublishOpt {
	if eventID := eventPublishID(event); eventID != "" {
		return []jetstream.PublishOpt{jetstream.WithMsgID(eventID)}
	}
	return nil
}

func eventPublishID(event Event) string {
	switch rec := event.(type) {
	case *LLMCallFinalizedEvent:
		return rec.EventID
	default:
		return ""
	}
}
