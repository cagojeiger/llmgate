package sink

import (
	"context"
	"errors"
	"testing"

	llmresultschema "llmgate/internal/domain/llmresult/schema"
)

type countSink struct {
	emits    int
	closed   bool
	doPanic  bool
	closeErr error
}

func (c *countSink) Emit(context.Context, *llmresultschema.Event) {
	if c.doPanic {
		panic("boom")
	}
	c.emits++
}

func (c *countSink) Close() error {
	c.closed = true
	return c.closeErr
}

func TestFanout_EmitsToAll(t *testing.T) {
	a, b := &countSink{}, &countSink{}
	f := NewFanoutSink(nil, a, b)
	f.Emit(context.Background(), &llmresultschema.Event{})
	if a.emits != 1 || b.emits != 1 {
		t.Fatalf("emits = (%d, %d), want (1, 1)", a.emits, b.emits)
	}
}

func TestFanout_PanicIsolation(t *testing.T) {
	bad, good := &countSink{doPanic: true}, &countSink{}
	f := NewFanoutSink(nil, bad, good)
	f.Emit(context.Background(), &llmresultschema.Event{}) // must not panic out
	if good.emits != 1 {
		t.Fatalf("good sink emits = %d, want 1 despite the other sink panicking", good.emits)
	}
}

func TestFanout_CloseAllReturnsFirstErr(t *testing.T) {
	a := &countSink{closeErr: errors.New("close failed")}
	b := &countSink{}
	f := NewFanoutSink(nil, a, b)
	if err := f.Close(); err == nil {
		t.Fatal("Close should surface the first sink error")
	}
	if !a.closed || !b.closed {
		t.Fatalf("both sinks should be closed: a=%v b=%v", a.closed, b.closed)
	}
}
