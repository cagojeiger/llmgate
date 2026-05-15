package audit

import (
	"time"

	"llmgate/internal/llmtypes"
)

// EventCommon holds fields shared by Record (operational audit) and
// CallRecord (LLM invocation business event). It is embedded in both
// types so schema changes to shared identity/request metadata propagate
// to both streams without duplication.
type EventCommon struct {
	RequestID     string
	Timestamp     time.Time
	ConsumerName  string
	ConsumerKeyID string
	Operation     string
	StatusCode    int
	Kind          llmtypes.ErrorKind
	DurationMS    int64
}
