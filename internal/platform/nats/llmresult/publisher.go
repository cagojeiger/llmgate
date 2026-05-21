package llmresult

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	natsgo "github.com/nats-io/nats.go"

	result "llmgate/internal/domain/llmresult/schema"
)

const (
	DefaultStream  = "LLMRESULT"
	DefaultSubject = "llmgate.llmresult.finalized"

	contentTypeJSON     = "application/json"
	headerContentType   = "Content-Type"
	headerEventType     = "X-LLMGate-Event-Type"
	headerRequestID     = "X-LLMGate-Request-ID"
	headerSchemaVersion = "X-LLMGate-Schema-Version"
)

type Config struct {
	URL     string
	Stream  string
	Subject string
}

type Publisher struct {
	nc      *natsgo.Conn
	js      jetStream
	stream  string
	subject string
	log     *slog.Logger
}

type jetStream interface {
	PublishMsg(m *natsgo.Msg, opts ...natsgo.PubOpt) (*natsgo.PubAck, error)
	StreamInfo(stream string, opts ...natsgo.JSOpt) (*natsgo.StreamInfo, error)
	AddStream(cfg *natsgo.StreamConfig, opts ...natsgo.JSOpt) (*natsgo.StreamInfo, error)
}

func NewPublisher(ctx context.Context, cfg Config, log *slog.Logger) (*Publisher, error) {
	cfg = cfg.withDefaults()
	if cfg.URL == "" {
		return nil, errors.New("nats url is required")
	}
	if log == nil {
		log = slog.Default()
	}
	nc, err := natsgo.Connect(cfg.URL, natsgo.Name("llmgate llmresult publisher"))
	if err != nil {
		return nil, fmt.Errorf("connect nats: %w", err)
	}
	js, err := nc.JetStream()
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("create jetstream context: %w", err)
	}
	p := &Publisher{nc: nc, js: js, stream: cfg.Stream, subject: cfg.Subject, log: log}
	if err := p.ensureStream(ctx); err != nil {
		nc.Close()
		return nil, err
	}
	return p, nil
}

func (c Config) withDefaults() Config {
	if c.Stream == "" {
		c.Stream = DefaultStream
	}
	if c.Subject == "" {
		c.Subject = DefaultSubject
	}
	return c
}

func newPublisher(js jetStream, stream, subject string) *Publisher {
	cfg := Config{Stream: stream, Subject: subject}.withDefaults()
	return &Publisher{js: js, stream: cfg.Stream, subject: cfg.Subject, log: slog.Default()}
}

func (p *Publisher) Emit(ctx context.Context, event *result.Event) {
	if p == nil || event == nil {
		return
	}
	if err := p.Publish(ctx, event); err != nil {
		p.log.LogAttrs(ctx, slog.LevelWarn, "publish llm result event failed",
			slog.String("event_type", eventTypeOf(event)),
			slog.String("request_id", event.RequestID),
			slog.String("err", err.Error()),
		)
	}
}

func (p *Publisher) Publish(ctx context.Context, event *result.Event) error {
	if p == nil || p.js == nil {
		return errors.New("nats publisher is not initialized")
	}
	if event == nil {
		return errors.New("llm result event is nil")
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal llm result event: %w", err)
	}
	nmsg := &natsgo.Msg{
		Subject: p.subject,
		Header:  natsgo.Header{},
		Data:    payload,
	}
	nmsg.Header.Set(headerContentType, contentTypeJSON)
	nmsg.Header.Set(headerEventType, event.EventType)
	nmsg.Header.Set(headerRequestID, event.RequestID)
	nmsg.Header.Set(headerSchemaVersion, strconv.Itoa(event.SchemaVersion))

	opts := []natsgo.PubOpt{natsgo.Context(ctx)}
	if event.RequestID != "" {
		opts = append(opts, natsgo.MsgId(event.RequestID))
	}
	if _, err := p.js.PublishMsg(nmsg, opts...); err != nil {
		return fmt.Errorf("publish jetstream message: %w", err)
	}
	return nil
}

func (p *Publisher) Close() error {
	if p == nil || p.nc == nil {
		return nil
	}
	p.nc.Close()
	return nil
}

func (p *Publisher) ensureStream(ctx context.Context) error {
	if p == nil || p.js == nil {
		return errors.New("nats publisher is not initialized")
	}
	_, err := p.js.StreamInfo(p.stream, natsgo.Context(ctx))
	if err == nil {
		return nil
	}
	if !errors.Is(err, natsgo.ErrStreamNotFound) {
		return fmt.Errorf("inspect jetstream stream %q: %w", p.stream, err)
	}
	_, err = p.js.AddStream(&natsgo.StreamConfig{
		Name:      p.stream,
		Subjects:  []string{p.subject},
		Retention: natsgo.LimitsPolicy,
		Storage:   natsgo.FileStorage,
		Discard:   natsgo.DiscardOld,
	}, natsgo.Context(ctx))
	if err != nil {
		return fmt.Errorf("create jetstream stream %q: %w", p.stream, err)
	}
	return nil
}

func eventTypeOf(event *result.Event) string {
	if event == nil {
		return ""
	}
	return event.AnalyticsEventType()
}
