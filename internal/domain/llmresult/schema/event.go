package schema

import (
	"encoding/json"
	"fmt"
	"time"

	"llmgate/internal/domain/llmtypes"
)

const (
	SchemaVersion = 1
	EventType     = "llm.result.finalized"
)

// Event is the durable analytics/training-data boundary for one finalized LLM
// request. It is separate from telemetry.CallEvent so operational metrics can
// stay small while downstream data pipelines receive the prompt/response body.
type Event struct {
	SchemaVersion int    `json:"schema_version"`
	EventType     string `json:"event_type"`

	Timestamp time.Time `json:"timestamp"`
	RequestID string    `json:"request_id"`

	ServiceName    string `json:"service_name,omitempty"`
	ServiceVersion string `json:"service_version,omitempty"`
	Environment    string `json:"environment,omitempty"`
	Operation      string `json:"operation"`

	ConsumerName  string `json:"consumer_name,omitempty"`
	ConsumerKeyID string `json:"consumer_key_id,omitempty"`

	StatusCode int                `json:"status"`
	ErrorKind  llmtypes.ErrorKind `json:"error_kind,omitempty"`
	DurationMS int64              `json:"duration_ms"`
	Request    *llmtypes.Request  `json:"request,omitempty"`
	Response   *llmtypes.Response `json:"response,omitempty"`
	Usage      *llmtypes.Usage    `json:"usage,omitempty"`
	Attempts   []llmtypes.Attempt `json:"attempts,omitempty"`

	ModelRequested string `json:"model_requested,omitempty"`
	ModelUsed      string `json:"model_used,omitempty"`
	Vendor         string `json:"vendor,omitempty"`

	RequestBytes  int64  `json:"request_bytes,omitempty"`
	ResponseBytes int64  `json:"response_bytes,omitempty"`
	VendorCost    string `json:"vendor_cost,omitempty"`

	FirstByteMS  int64 `json:"first_byte_ms,omitempty"`
	StreamChunks int   `json:"stream_chunks,omitempty"`
}

func (e *Event) AnalyticsEventType() string {
	if e == nil {
		return ""
	}
	return e.EventType
}

func cloneAttempts(attempts []llmtypes.Attempt) []llmtypes.Attempt {
	if len(attempts) == 0 {
		return nil
	}
	out, err := json.Marshal(attempts)
	if err != nil {
		panic(fmt.Errorf("schema: marshal attempts for clone: %w", err))
	}
	var cloned []llmtypes.Attempt
	if err := json.Unmarshal(out, &cloned); err != nil {
		panic(fmt.Errorf("schema: unmarshal attempts for clone: %w", err))
	}
	return cloned
}

// cloneJSON deep-copies via the encoding/json round-trip. The schema
// types (Request / Response) only contain primitives, strings, slices,
// maps, and json.RawMessage, so this round-trip is total — a marshal
// failure can only mean a new field type slipped in that encoding/json
// cannot serialize, which we want surfaced in tests/CI rather than
// silently dropping the durable audit payload at runtime.
func cloneJSON[T any](in *T) *T {
	if in == nil {
		return nil
	}
	out, err := json.Marshal(in)
	if err != nil {
		panic(fmt.Errorf("schema: marshal for clone: %w", err))
	}
	var cloned T
	if err := json.Unmarshal(out, &cloned); err != nil {
		panic(fmt.Errorf("schema: unmarshal for clone: %w", err))
	}
	return &cloned
}
