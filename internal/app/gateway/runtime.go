package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"llmgate/internal/domain/catalog"
	"llmgate/internal/domain/consumers"
	llmresultsink "llmgate/internal/domain/llmresult/sink"
	"llmgate/internal/domain/routing"
	"llmgate/internal/domain/telemetry"
	"llmgate/internal/platform/config"
	httpchat "llmgate/internal/platform/http/chat"
	httpprobe "llmgate/internal/platform/http/probe"
	"llmgate/internal/platform/http/server"
	promtelemetry "llmgate/internal/platform/telemetry/prometheus"
	slogtelemetry "llmgate/internal/platform/telemetry/slog"
)

type RuntimeInput struct {
	Config    *config.Server
	Catalog   *catalog.Catalog
	Consumers *consumers.Store
	Logger    *slog.Logger
	Version   string
}

type LoadInput struct {
	Config  *config.Server
	Logger  *slog.Logger
	Version string
}

type Runtime struct {
	Server *http.Server
	Probe  *httpprobe.State

	cfg     *config.Server
	log     *slog.Logger
	events  telemetry.EventSink
	results llmresultsink.Sink
}

func LoadRuntime(ctx context.Context, in LoadInput) (*Runtime, error) {
	log := in.Logger
	if log == nil {
		log = slog.Default()
	}
	cat, err := catalog.Load()
	if err != nil {
		return nil, fmt.Errorf("load catalog: %w", err)
	}
	log.Info("catalog loaded",
		slog.Int("models", len(cat.Models)),
		slog.Int("aliases", len(cat.Aliases)),
	)

	consumerStore, err := consumers.Load()
	if err != nil {
		return nil, fmt.Errorf("load consumers: %w", err)
	}
	log.Info("consumers loaded", slog.Int("consumers", consumerStore.Len()))

	return BuildRuntime(ctx, RuntimeInput{
		Config:    in.Config,
		Catalog:   cat,
		Consumers: consumerStore,
		Logger:    log,
		Version:   in.Version,
	})
}

func BuildRuntime(ctx context.Context, in RuntimeInput) (*Runtime, error) {
	if in.Config == nil {
		return nil, errors.New("gateway runtime config is required")
	}
	if in.Catalog == nil {
		return nil, errors.New("gateway runtime catalog is required")
	}
	if in.Consumers == nil {
		return nil, errors.New("gateway runtime consumers are required")
	}
	if in.Logger == nil {
		in.Logger = slog.Default()
	}

	models, aliases, err := buildRouterInputs(in.Catalog, defaultProviderFactories())
	if err != nil {
		return nil, err
	}

	policy := routing.FallbackPolicy{
		OnKinds:         in.Config.FallbackOn,
		CircuitFailures: in.Config.CircuitFailures,
		CircuitOpen:     in.Config.CircuitOpen,
		CircuitMaxOpen:  in.Config.CircuitMaxOpen,
		CircuitJitter:   in.Config.CircuitJitter,
		CompleteTimeout: in.Config.CompleteTimeout,
	}
	svc, err := routing.NewService(models, aliases, policy, in.Logger)
	if err != nil {
		return nil, err
	}

	metricsRegistry := prometheus.NewRegistry()
	metricsRegistry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	metricsRecorder, err := promtelemetry.NewRecorder(metricsRegistry)
	if err != nil {
		return nil, fmt.Errorf("build prometheus recorder: %w", err)
	}

	auditLog := in.Logger.With(slog.String("log", "audit"))
	callLog := in.Logger.With(slog.String("log", "call"))
	events := telemetry.NewFanoutSink(in.Logger,
		slogtelemetry.NewSink(auditLog, callLog),
		metricsRecorder,
	)

	results, err := buildResultSink(ctx, in.Config, in.Logger, metricsRecorder)
	if err != nil {
		_ = events.Close()
		return nil, err
	}

	handler := httpchat.NewHandler(svc, in.Logger, events, httpchat.HandlerConfig{
		RequestTimeout:    in.Config.RequestTimeout,
		StreamIdleTimeout: in.Config.StreamIdleTimeout,
		ServiceVersion:    in.Version,
		Environment:       in.Config.Environment,
		LifecycleObserver: metricsRecorder,
		ResultSink:        results,
		ResultPayloadMode: in.Config.LLMResultPayloadMode,
	})
	probe := httpprobe.NewState()
	var metricsHandler http.Handler
	if in.Config.MetricsEnabled {
		metricsHandler = promhttp.HandlerFor(metricsRegistry, promhttp.HandlerOpts{
			MaxRequestsInFlight: 5,
			Timeout:             5 * time.Second,
		})
	}
	srv := server.NewWithOptions(server.ServerOptions{
		Config:         in.Config,
		Log:            in.Logger.With(slog.String("log", "access")),
		Handler:        handler,
		Consumers:      in.Consumers,
		Probe:          probe,
		MetricsHandler: metricsHandler,
	})

	return &Runtime{
		Server:  srv,
		Probe:   probe,
		cfg:     in.Config,
		log:     in.Logger,
		events:  events,
		results: results,
	}, nil
}
