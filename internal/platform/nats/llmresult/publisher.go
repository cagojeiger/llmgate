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
	DefaultSubject = "llmgate.llmresult.finalized"

	contentTypeJSON     = "application/json"
	headerContentType   = "Content-Type"
	headerEventType     = "X-LLMGate-Event-Type"
	headerRequestID     = "X-LLMGate-Request-ID"
	headerSchemaVersion = "X-LLMGate-Schema-Version"
)

type Config struct {
	URL      string
	Subject  string
	User     string
	Password string
}

type Publisher struct {
	nc      *natsgo.Conn
	js      jetStream
	subject string
	log     *slog.Logger
}

type jetStream interface {
	PublishMsg(m *natsgo.Msg, opts ...natsgo.PubOpt) (*natsgo.PubAck, error)
}

func NewPublisher(ctx context.Context, cfg Config, log *slog.Logger) (*Publisher, error) {
	cfg = cfg.withDefaults()
	if cfg.URL == "" {
		return nil, errors.New("nats url is required")
	}
	if log == nil {
		log = slog.Default()
	}
	opts := []natsgo.Option{natsgo.Name("llmgate llmresult publisher")}
	if cfg.User != "" {
		opts = append(opts, natsgo.UserInfo(cfg.User, cfg.Password))
	}
	nc, err := natsgo.Connect(cfg.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("connect nats: %w", err)
	}
	js, err := nc.JetStream()
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("create jetstream context: %w", err)
	}
	return &Publisher{nc: nc, js: js, subject: cfg.Subject, log: log}, nil
}

func (c Config) withDefaults() Config {
	if c.Subject == "" {
		c.Subject = DefaultSubject
	}
	return c
}

func newPublisher(js jetStream, subject string) *Publisher {
	cfg := Config{Subject: subject}.withDefaults()
	return &Publisher{js: js, subject: cfg.Subject, log: slog.Default()}
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

func eventTypeOf(event *result.Event) string {
	if event == nil {
		return ""
	}
	return event.AnalyticsEventType()
}
