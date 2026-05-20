package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"llmgate/internal/catalog"
	"llmgate/internal/config"
	"llmgate/internal/consumers"
	llmresultsink "llmgate/internal/domain/llmresult/sink"
	"llmgate/internal/llmrouter"
	"llmgate/internal/server"
	"llmgate/internal/telemetry"
)

type RuntimeInput struct {
	Config    *config.Server
	Catalog   *catalog.Catalog
	Consumers *consumers.Store
	Logger    *slog.Logger
	Version   string
}

type Runtime struct {
	Server *http.Server
	Probe  *server.ProbeState

	log     *slog.Logger
	events  telemetry.EventSink
	results llmresultsink.Sink
}

func BuildRuntime(ctx context.Context, in RuntimeInput) (*Runtime, error) {
	if in.Config == nil {
		return nil, fmt.Errorf("gateway runtime config is required")
	}
	if in.Catalog == nil {
		return nil, fmt.Errorf("gateway runtime catalog is required")
	}
	if in.Consumers == nil {
		return nil, fmt.Errorf("gateway runtime consumers are required")
	}
	if in.Logger == nil {
		in.Logger = slog.Default()
	}

	models, aliases, err := BuildRouterInputs(in.Catalog)
	if err != nil {
		return nil, err
	}

	policy := llmrouter.FallbackPolicy{
		OnKinds:         in.Config.FallbackOn,
		CircuitFailures: in.Config.CircuitFailures,
		CircuitOpen:     in.Config.CircuitOpen,
		CircuitMaxOpen:  in.Config.CircuitMaxOpen,
		CircuitJitter:   in.Config.CircuitJitter,
		CompleteTimeout: in.Config.CompleteTimeout,
	}
	svc, err := llmrouter.NewService(models, aliases, policy, in.Logger)
	if err != nil {
		return nil, err
	}

	metricsRegistry := prometheus.NewRegistry()
	metricsRegistry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	metricsRecorder, err := telemetry.NewPrometheusRecorder(metricsRegistry)
	if err != nil {
		return nil, fmt.Errorf("build prometheus recorder: %w", err)
	}

	auditLog := in.Logger.With(slog.String("log", "audit"))
	callLog := in.Logger.With(slog.String("log", "call"))
	events := telemetry.NewFanoutSink(in.Logger,
		telemetry.NewSlogSink(auditLog, callLog),
		metricsRecorder,
	)

	results, err := BuildResultSink(ctx, in.Config, in.Logger)
	if err != nil {
		_ = events.Close()
		return nil, err
	}

	handler := server.NewHandler(svc, in.Logger, events, server.HandlerConfig{
		RequestTimeout:    in.Config.RequestTimeout,
		StreamIdleTimeout: in.Config.StreamIdleTimeout,
		ServiceVersion:    in.Version,
		Environment:       in.Config.Environment,
		LifecycleObserver: metricsRecorder,
		ResultSink:        results,
	})
	probe := server.NewProbeState()
	srv := server.NewWithOptions(server.ServerOptions{
		Config:    in.Config,
		Log:       in.Logger.With(slog.String("log", "access")),
		Handler:   handler,
		Consumers: in.Consumers,
		Probe:     probe,
		MetricsHandler: promhttp.HandlerFor(metricsRegistry, promhttp.HandlerOpts{
			MaxRequestsInFlight: 5,
			Timeout:             5 * time.Second,
		}),
	})

	return &Runtime{
		Server:  srv,
		Probe:   probe,
		log:     in.Logger,
		events:  events,
		results: results,
	}, nil
}

func (r *Runtime) Close() error {
	if r == nil {
		return nil
	}
	log := r.log
	if log == nil {
		log = slog.Default()
	}
	var first error
	if r.results != nil {
		if err := r.results.Close(); err != nil {
			log.Warn("llm result sink close failed", slog.String("err", err.Error()))
			first = err
		}
	}
	if r.events != nil {
		if err := r.events.Close(); err != nil {
			log.Warn("telemetry sink close failed", slog.String("err", err.Error()))
			if first == nil {
				first = err
			}
		}
	}
	return first
}
