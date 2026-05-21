package promtelemetry

import (
	"context"

	"llmgate/internal/domain/telemetry"
)

func (r *Recorder) RequestStarted(context.Context) {
	if r != nil {
		r.inflightRequests.Inc()
	}
}

func (r *Recorder) RequestFinished(context.Context) {
	if r != nil {
		r.inflightRequests.Dec()
	}
}

func (r *Recorder) StreamStarted(context.Context, telemetry.EventCommon) {
	if r != nil {
		r.inflightStreams.Inc()
	}
}

func (r *Recorder) StreamFinished(context.Context, *telemetry.AuditEvent, *telemetry.CallEvent) {
	if r != nil {
		r.inflightStreams.Dec()
	}
}

func (r *Recorder) Close() error { return nil }
