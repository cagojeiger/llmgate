package telemetry

import (
	"time"

	"llmgate/internal/domain/llmtypes"
)

const (
	ServiceName = "llmgate"

	SchemaVersion  = 1
	EventTypeAudit = "audit"
	EventTypeCall  = "call"
)

// EventCommon holds fields shared by operational audit records and LLM
// call-result records. request_id is the join key across access / audit / call.
type EventCommon struct {
	Timestamp time.Time
	RequestID string

	ServiceName    string
	ServiceVersion string
	Environment    string

	// Operation is the gateway-domain method name —
	// "chat.completions" or "chat.completions.stream" — not the HTTP
	// method. Naming it Method would clash with r.Method (HTTP "POST")
	// in mixed log streams.
	Operation string

	// ConsumerName identifies the registered caller (matched yaml `name` in
	// consumers/) for this request. Empty when the request was rejected at
	// the auth boundary; the record is still emitted in that case so
	// brute-force / mis-configured-key activity is observable. ConsumerKeyID
	// is the first 8 hex chars of the matched hash (sha256), useful for
	// detecting which key in a rotating set was actually used.
	ConsumerName  string
	ConsumerKeyID string

	StatusCode int
	Kind       llmtypes.ErrorKind
	DurationMS int64
}

// CommonInput is the request-boundary data needed to start telemetry events.
type CommonInput struct {
	Timestamp      time.Time
	RequestID      string
	ServiceName    string
	ServiceVersion string
	Environment    string
	Operation      string
	ConsumerName   string
	ConsumerKeyID  string
}

func NewEventCommon(in CommonInput) EventCommon {
	serviceName := in.ServiceName
	if serviceName == "" {
		serviceName = ServiceName
	}
	serviceVersion := in.ServiceVersion
	if serviceVersion == "" {
		serviceVersion = "dev"
	}
	environment := in.Environment
	if environment == "" {
		environment = "local"
	}
	return EventCommon{
		Timestamp:      in.Timestamp,
		RequestID:      in.RequestID,
		ServiceName:    serviceName,
		ServiceVersion: serviceVersion,
		Environment:    environment,
		Operation:      in.Operation,
		ConsumerName:   in.ConsumerName,
		ConsumerKeyID:  in.ConsumerKeyID,
	}
}
