package telemetry

import (
	"bytes"
	"context"
	"log/slog"
)

func newCapturingLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), buf
}

type panicSink struct{}

func (panicSink) Emit(context.Context, Event) { panic("sink failed") }
func (panicSink) Close() error                { return nil }

type captureSink struct {
	events []Event
}

func (c *captureSink) Emit(_ context.Context, event Event) {
	c.events = append(c.events, event)
}

func (c *captureSink) Close() error { return nil }

type closeErrSink struct {
	err error
}

func (c closeErrSink) Emit(context.Context, Event) {}
func (c closeErrSink) Close() error                { return c.err }

type captureLifecycleObserver struct {
	requestStarted  int
	requestFinished int
	streamStarted   int
	streamFinished  int
}

func (c *captureLifecycleObserver) RequestStarted(context.Context) {
	c.requestStarted++
}

func (c *captureLifecycleObserver) RequestFinished(context.Context) {
	c.requestFinished++
}

func (c *captureLifecycleObserver) StreamStarted(context.Context, EventCommon) {
	c.streamStarted++
}

func (c *captureLifecycleObserver) StreamFinished(context.Context, *AuditEvent, *CallEvent) {
	c.streamFinished++
}

type panicLifecycleObserver struct{}

func (panicLifecycleObserver) RequestStarted(context.Context)             { panic("request started") }
func (panicLifecycleObserver) RequestFinished(context.Context)            { panic("request finished") }
func (panicLifecycleObserver) StreamStarted(context.Context, EventCommon) { panic("stream started") }
func (panicLifecycleObserver) StreamFinished(context.Context, *AuditEvent, *CallEvent) {
	panic("stream finished")
}
