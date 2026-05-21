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
	Probe  *server.ProbeState

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

	results, err := BuildResultSink(ctx, in.Config, in.Logger)
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
		cfg:     in.Config,
		log:     in.Logger,
		events:  events,
		results: results,
	}, nil
}

func (r *Runtime) Run(ctx context.Context) error {
	if r == nil || r.Server == nil {
		return fmt.Errorf("gateway runtime server is required")
	}
	log := r.logger()
	errCh := make(chan error, 1)
	go func() {
		log.Info("server listening", slog.String("addr", r.Server.Addr))
		err := r.Server.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errCh:
	}

	if r.Probe != nil {
		r.Probe.MarkShuttingDown()
	}
	r.shutdown()
	if serveErr != nil {
		return serveErr
	}
	return nil
}

func (r *Runtime) Close() error {
	if r == nil {
		return nil
	}
	log := r.logger()
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

// shutdown drains in-flight requests until either the server reports done or
// ShutdownDrainTimeout elapses. Readiness must be flipped before this method.
func (r *Runtime) shutdown() {
	if r == nil || r.Server == nil || r.cfg == nil {
		return
	}
	log := r.logger()
	log.Info("shutdown initiated; draining in-flight requests",
		slog.Duration("max_wait", r.cfg.ShutdownDrainTimeout))

	ctx, cancel := context.WithTimeout(context.Background(), r.cfg.ShutdownDrainTimeout)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- r.Server.Shutdown(ctx) }()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	start := time.Now()

	for {
		select {
		case err := <-done:
			elapsed := time.Since(start)
			if errors.Is(err, context.DeadlineExceeded) {
				log.Warn("drain deadline exceeded; force closing remaining connections",
					slog.Duration("waited", elapsed))
				if closeErr := r.Server.Close(); closeErr != nil && !errors.Is(closeErr, http.ErrServerClosed) {
					log.Warn("force close failed", slog.String("err", closeErr.Error()))
				}
				return
			}
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Warn("shutdown returned error", slog.String("err", err.Error()))
			}
			log.Info("shutdown complete", slog.Int64("duration_ms", elapsed.Milliseconds()))
			return
		case <-ticker.C:
			log.Info("still draining...", slog.Int64("elapsed_ms", time.Since(start).Milliseconds()))
		}
	}
}

func (r *Runtime) logger() *slog.Logger {
	if r == nil || r.log == nil {
		return slog.Default()
	}
	return r.log
}
