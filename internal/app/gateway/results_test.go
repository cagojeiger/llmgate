package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	llmresultschema "llmgate/internal/domain/llmresult/schema"
	llmresultsink "llmgate/internal/domain/llmresult/sink"
	"llmgate/internal/platform/config"
)

func TestBuildResultSink_DisabledByDefault(t *testing.T) {
	got, err := buildResultSink(context.Background(), &config.Server{}, discardLogger())
	if err != nil {
		t.Fatalf("buildResultSink() error = %v", err)
	}
	if _, ok := got.(llmresultsink.NopSink); !ok {
		t.Fatalf("sink type = %T, want NopSink", got)
	}
}

type stubResultSink struct{}

func (stubResultSink) Emit(context.Context, *llmresultschema.Event) {}
func (stubResultSink) Close() error                                 { return nil }

func stubFactory(name string, on bool) resultSinkFactory {
	return resultSinkFactory{
		name:    name,
		enabled: func(*config.Server) bool { return on },
		build: func(context.Context, *config.Server, *slog.Logger) (llmresultsink.Sink, error) {
			return stubResultSink{}, nil
		},
	}
}

func TestAssembleResultSink_ByEnabledCount(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Server{}
	log := discardLogger()

	// none enabled -> Nop
	got, err := assembleResultSink(ctx, cfg, log, []resultSinkFactory{stubFactory("a", false)})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if _, ok := got.(llmresultsink.NopSink); !ok {
		t.Fatalf("0 enabled: got %T, want NopSink", got)
	}

	// one enabled -> single AsyncSink (no fan-out overhead)
	got, err = assembleResultSink(ctx, cfg, log, []resultSinkFactory{stubFactory("a", true), stubFactory("b", false)})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if _, ok := got.(*llmresultsink.AsyncSink); !ok {
		t.Fatalf("1 enabled: got %T, want *AsyncSink", got)
	}
	_ = got.Close()

	// two enabled -> still one async boundary (fan-out lives inside it)
	got, err = assembleResultSink(ctx, cfg, log, []resultSinkFactory{stubFactory("a", true), stubFactory("b", true)})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if _, ok := got.(*llmresultsink.AsyncSink); !ok {
		t.Fatalf("2 enabled: got %T, want *AsyncSink (single boundary wrapping the fan-out)", got)
	}
	_ = got.Close()
}

type countingSink struct {
	mu sync.Mutex
	n  int
}

func (c *countingSink) Emit(context.Context, *llmresultschema.Event) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}
func (c *countingSink) Close() error { return nil }
func (c *countingSink) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func termFactory(name string, on bool, sink llmresultsink.Sink) resultSinkFactory {
	return resultSinkFactory{
		name:    name,
		enabled: func(*config.Server) bool { return on },
		build: func(context.Context, *config.Server, *slog.Logger) (llmresultsink.Sink, error) {
			return sink, nil
		},
	}
}

// With two terminals, one async boundary must fan out to both. Close
// drains the worker queue, so the assertion is deterministic.
func TestAssembleResultSink_SingleBoundaryFansOutToAll(t *testing.T) {
	a, b := &countingSink{}, &countingSink{}
	sink, err := assembleResultSink(context.Background(), &config.Server{}, discardLogger(),
		[]resultSinkFactory{termFactory("a", true, a), termFactory("b", true, b)})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	sink.Emit(context.Background(), &llmresultschema.Event{})
	if err := sink.Close(); err != nil { // drains the queue
		t.Fatalf("close: %v", err)
	}
	if a.count() != 1 || b.count() != 1 {
		t.Fatalf("fan-out delivery = (%d, %d), want (1, 1)", a.count(), b.count())
	}
}

func TestAssembleResultSink_BuildErrorPropagates(t *testing.T) {
	boom := resultSinkFactory{
		name:    "boom",
		enabled: func(*config.Server) bool { return true },
		build: func(context.Context, *config.Server, *slog.Logger) (llmresultsink.Sink, error) {
			return nil, errors.New("build failed")
		},
	}
	_, err := assembleResultSink(context.Background(), &config.Server{}, discardLogger(), []resultSinkFactory{boom})
	if err == nil {
		t.Fatal("assembleResultSink should propagate a factory build error")
	}
}

func TestDefaultResultSinkFactories_NamesAndGates(t *testing.T) {
	factories := defaultResultSinkFactories()
	if len(factories) != 2 {
		t.Fatalf("factory count = %d, want 2 (audit, nats)", len(factories))
	}
	audit, nats := factories[0], factories[1]
	if audit.name != "audit" || nats.name != "nats" {
		t.Fatalf("names = (%q, %q), want (audit, nats)", audit.name, nats.name)
	}
	if !audit.enabled(&config.Server{AuditDir: "/x"}) || audit.enabled(&config.Server{}) {
		t.Fatal("audit gate should key off AuditDir")
	}
	if !nats.enabled(&config.Server{LLMResultNATSURL: "nats://x"}) || nats.enabled(&config.Server{}) {
		t.Fatal("nats gate should key off LLMResultNATSURL")
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
