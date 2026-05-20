package server

import (
	"context"

	"llmgate/internal/events/llmresult"
	llmresultsink "llmgate/internal/events/llmresult/sink"
	"llmgate/internal/llmtypes"
	"llmgate/internal/telemetry"
)

type resultRecorder struct {
	sink     llmresultsink.Sink
	request  *llmtypes.Request
	response *llmtypes.Response
}

func newResultRecorder(sink llmresultsink.Sink) *resultRecorder {
	return &resultRecorder{sink: sink}
}

func (r *resultRecorder) Request(req *llmtypes.Request) {
	r.request = req
}

func (r *resultRecorder) Response(resp *llmtypes.Response) {
	r.response = resp
}

func (r *resultRecorder) Emit(ctx context.Context, audit *telemetry.AuditEvent, call *telemetry.CallEvent) {
	if r == nil || r.sink == nil {
		return
	}
	ev, ok := llmresult.FromTelemetry(llmresult.BuildInput{
		Audit:    audit,
		Call:     call,
		Request:  r.request,
		Response: r.response,
	})
	if !ok {
		return
	}
	r.sink.Emit(ctx, ev)
}
