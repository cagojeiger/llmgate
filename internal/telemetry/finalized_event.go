package telemetry

import (
	"encoding/json"

	"llmgate/internal/llmtypes"
)

const (
	LLMCallFinalizedSchemaVersion = 1
	LLMCallFinalizedSubject       = "llmgate.llm.results.v1"
	LLMCallFinalizedWireFormat    = "openai.chat.completions"
)

type LLMCallStatus string

const (
	LLMCallStatusSuccess      LLMCallStatus = "success"
	LLMCallStatusError        LLMCallStatus = "error"
	LLMCallStatusPartial      LLMCallStatus = "partial"
	LLMCallStatusClientClosed LLMCallStatus = "client_closed"
)

// LLMCallFinalizedEvent is the raw event contract for downstream cost,
// dataset, and archive consumers. It records the OpenAI-compatible wire shape
// llmgate received and returned, not vendor-native payloads.
type LLMCallFinalizedEvent struct {
	SchemaVersion int           `json:"schema_version"`
	EventType     string        `json:"event_type"`
	EventID       string        `json:"event_id"`
	RequestID     string        `json:"request_id"`
	Timestamp     string        `json:"timestamp"`
	CompletedAt   string        `json:"completed_at,omitempty"`
	DurationMS    int64         `json:"duration_ms,omitempty"`
	Status        LLMCallStatus `json:"status"`
	Operation     string        `json:"operation"`
	WireFormat    string        `json:"wire_format"`

	Service  EventService    `json:"service"`
	Consumer EventConsumer   `json:"consumer,omitempty"`
	Routing  EventRouting    `json:"routing,omitempty"`
	Request  RawEnvelope     `json:"request"`
	Response RawEnvelope     `json:"response"`
	Stream   StreamEnvelope  `json:"stream"`
	Usage    *llmtypes.Usage `json:"usage,omitempty"`
	Error    *EventError     `json:"error,omitempty"`
}

type EventService struct {
	Name        string `json:"name,omitempty"`
	Version     string `json:"version,omitempty"`
	Environment string `json:"environment,omitempty"`
}

type EventConsumer struct {
	Name  string `json:"name,omitempty"`
	KeyID string `json:"key_id,omitempty"`
}

type EventRouting struct {
	ModelRequested string         `json:"model_requested,omitempty"`
	ModelUsed      string         `json:"model_used,omitempty"`
	Vendor         string         `json:"vendor,omitempty"`
	Attempts       []EventAttempt `json:"attempts,omitempty"`
}

type EventAttempt struct {
	Vendor     string          `json:"vendor,omitempty"`
	Model      string          `json:"model,omitempty"`
	StartedAt  string          `json:"started_at,omitempty"`
	DurationMS int64           `json:"duration_ms,omitempty"`
	StatusCode int             `json:"status_code,omitempty"`
	Kind       string          `json:"error_kind,omitempty"`
	Usage      *llmtypes.Usage `json:"usage,omitempty"`
	VendorCost string          `json:"vendor_cost,omitempty"`
}

type RawEnvelope struct {
	Available bool            `json:"available"`
	RawJSON   json.RawMessage `json:"raw_json,omitempty"`
	Reason    string          `json:"reason,omitempty"`
}

type StreamEnvelope struct {
	Enabled            bool              `json:"enabled"`
	EventsAvailable    bool              `json:"events_available,omitempty"`
	Events             []json.RawMessage `json:"events,omitempty"`
	RawEventsTruncated bool              `json:"raw_events_truncated,omitempty"`
}

type EventError struct {
	Kind    string `json:"kind,omitempty"`
	Message string `json:"message,omitempty"`
}

func (*LLMCallFinalizedEvent) TelemetryEventType() string { return EventTypeLLMCallFinalized }

func NewLLMCallFinalizedEvent(common EventCommon) *LLMCallFinalizedEvent {
	return &LLMCallFinalizedEvent{
		SchemaVersion: LLMCallFinalizedSchemaVersion,
		EventType:     EventTypeLLMCallFinalized,
		EventID:       common.RequestID + ":llm-call-finalized:v1",
		RequestID:     common.RequestID,
		Timestamp:     common.Timestamp.UTC().Format("2006-01-02T15:04:05.000000000Z"),
		Operation:     common.Operation,
		WireFormat:    LLMCallFinalizedWireFormat,
		Service: EventService{
			Name:        common.ServiceName,
			Version:     common.ServiceVersion,
			Environment: common.Environment,
		},
		Consumer: EventConsumer{
			Name:  common.ConsumerName,
			KeyID: common.ConsumerKeyID,
		},
	}
}
