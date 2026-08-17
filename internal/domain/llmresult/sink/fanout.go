package sink

import (
	"log/slog"

	llmresultschema "llmgate/internal/domain/llmresult/schema"
	"llmgate/internal/domain/sinkutil"
)

// FanoutSink delivers each finalized event to every contained sink. It
// lets multiple result sinks run side by side. The fan-out logic is the
// shared sinkutil.Fanout; this alias pins the pipeline's event type,
// panic label, and event-type accessor.
type FanoutSink = sinkutil.Fanout[*llmresultschema.Event]

func NewFanoutSink(log *slog.Logger, sinks ...Sink) *FanoutSink {
	return sinkutil.NewFanout(log, "llm result sink panic", eventTypeOf, sinks...)
}
