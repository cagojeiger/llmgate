package sink

import (
	"log/slog"

	llmresultschema "llmgate/internal/domain/llmresult/schema"
	"llmgate/internal/domain/sinkutil"
)

// Sink receives finalized LLM result events. Implementations must keep
// transport backpressure out of the request path.
type Sink = sinkutil.Sink[*llmresultschema.Event]

// NopSink drops finalized result events.
type NopSink = sinkutil.Nop[*llmresultschema.Event]

// NewRecoveringSink wraps next so a panic in Emit is logged and isolated
// from the HTTP request path.
func NewRecoveringSink(next Sink, log *slog.Logger) Sink {
	return sinkutil.NewRecovering(next, log, "llm result sink panic", eventTypeOf)
}

func eventTypeOf(event *llmresultschema.Event) string {
	if event == nil {
		return ""
	}
	return event.AnalyticsEventType()
}
