package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
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

	// two enabled -> Fanout
	got, err = assembleResultSink(ctx, cfg, log, []resultSinkFactory{stubFactory("a", true), stubFactory("b", true)})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if _, ok := got.(*llmresultsink.FanoutSink); !ok {
		t.Fatalf("2 enabled: got %T, want *FanoutSink", got)
	}
	_ = got.Close()
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
