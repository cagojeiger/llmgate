package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"llmgate/internal/audit"
	"llmgate/internal/catalog"
	"llmgate/internal/consumers"
	"llmgate/internal/config"
	"llmgate/internal/provider"
	"llmgate/internal/provider/anthropic"
	"llmgate/internal/provider/openai"
	"llmgate/internal/dispatch"
	"llmgate/internal/server"
)

func main() {
	if err := run(); err != nil {
		slog.Error("llmgate failed", slog.String("err", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	_ = godotenv.Load()

	cfg, err := config.LoadServer()
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})).With(
		slog.String("service", "llmgate"),
		slog.String("phase", "v1-bypass"),
	)
	slog.SetDefault(logger)

	cat, err := catalog.Load()
	if err != nil {
		return fmt.Errorf("load catalog: %w", err)
	}
	logger.Info("catalog loaded",
		slog.Int("models", len(cat.Models)),
		slog.Int("aliases", len(cat.Aliases)),
	)

	consumerStore, err := consumers.Load()
	if err != nil {
		return fmt.Errorf("load consumers: %w", err)
	}
	logger.Info("consumers loaded", slog.Int("consumers", consumerStore.Len()))

	factories := map[string]providerFactory{
		"openai":    openaiFactory,
		"anthropic": anthropicFactory,
	}
	models, aliases, err := buildDispatcherInputs(cat, factories)
	if err != nil {
		return err
	}

	policy := dispatch.FallbackPolicy{
		OnKinds:         cfg.FallbackOn,
		CircuitFailures: cfg.CircuitFailures,
		CircuitOpen:     cfg.CircuitOpen,
		CircuitMaxOpen:  cfg.CircuitMaxOpen,
		CircuitJitter:   cfg.CircuitJitter,
		CompleteTimeout: cfg.CompleteTimeout,
	}
	rtr, err := dispatch.NewDispatcher(models, aliases, policy, logger)
	if err != nil {
		return err
	}

	recorder := audit.Composite{audit.NewLogRecorder(logger)}
	defer func() {
		if err := recorder.Close(); err != nil {
			logger.Warn("recorder close failed", slog.String("err", err.Error()))
		}
	}()

	handler := server.NewHandler(rtr, logger, recorder, server.HandlerConfig{
		RequestTimeout:    cfg.RequestTimeout,
		StreamIdleTimeout: cfg.StreamIdleTimeout,
	})
	probe := server.NewProbeState()
	srv := server.New(cfg, logger, handler, consumerStore, probe)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("server listening", slog.String("addr", cfg.Addr))
		err := srv.ListenAndServe()
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
	stop()

	// Flip readiness *before* the drain phase so the k8s endpoint
	// controller (and any HTTP load balancer) drop this pod from the
	// service first. Idempotent — safe even if shutdown was triggered
	// by a server-side error rather than SIGTERM.
	probe.MarkShuttingDown()
	shutdown(srv, cfg, logger)
	if serveErr != nil {
		return serveErr
	}
	return nil
}

// providerFactory builds the Provider for one catalog model. Lives in
// the cmd binary because it bridges two boundaries the dispatch package
// deliberately does not know about: the catalog yaml shape (catalog.Model
// fields) and the env-driven credential lookup (auth_env).
type providerFactory func(*catalog.Model) (provider.Provider, error)

// buildDispatcherInputs walks the catalog and turns it into the runtime
// shape the dispatcher expects: model id → already-instantiated Provider,
// alias name → ordered chain of model ids. The dispatcher itself stays
// catalog-agnostic; this helper is the single point that bridges the
// yaml shape into the service.
func buildDispatcherInputs(cat *catalog.Catalog, factories map[string]providerFactory) (dispatch.Models, dispatch.Aliases, error) {
	models := make(dispatch.Models, len(cat.Models))
	for id, m := range cat.Models {
		f, ok := factories[m.Protocol]
		if !ok {
			return nil, nil, fmt.Errorf("no adapter for protocol %q (model %q)", m.Protocol, m.ID)
		}
		p, err := f(m)
		if err != nil {
			return nil, nil, fmt.Errorf("build adapter for model %q protocol %q: %w", m.ID, m.Protocol, err)
		}
		models[id] = p
	}
	aliases := make(dispatch.Aliases, len(cat.Aliases))
	for name, a := range cat.Aliases {
		aliases[name] = append([]string(nil), a.Chain...)
	}
	return models, aliases, nil
}

func openaiFactory(m *catalog.Model) (provider.Provider, error) {
	apiKey, err := readAuthKey(m)
	if err != nil {
		return nil, err
	}
	return openai.New(openai.Config{
		BaseURL:    m.BaseURL,
		APIKey:     apiKey,
		AuthScheme: m.AuthScheme,
		Name:       m.Vendor,
	})
}

func anthropicFactory(m *catalog.Model) (provider.Provider, error) {
	apiKey, err := readAuthKey(m)
	if err != nil {
		return nil, err
	}
	return anthropic.New(anthropic.Config{
		BaseURL:    m.BaseURL,
		APIKey:     apiKey,
		AuthScheme: m.AuthScheme,
		Name:       m.Vendor,
	})
}

// readAuthKey resolves the credential env var named by the catalog model.
func readAuthKey(m *catalog.Model) (string, error) {
	v := os.Getenv(m.AuthEnv)
	if v == "" {
		return "", fmt.Errorf("model %q: env %s is unset", m.ID, m.AuthEnv)
	}
	return v, nil
}

// shutdown drains in-flight requests until either the server reports
// done or ShutdownDrainTimeout elapses. The orchestrator's
// terminationGracePeriodSeconds (k8s) / stop_grace_period (compose)
// should be set slightly larger than ShutdownDrainTimeout so the
// app-side force close fires before SIGKILL — that way mid-stream
// connections close cleanly with an audit record rather than abruptly.
// A 5s ticker logs progress so an unusually long drain (a stuck stream,
// a misconfigured caller) is observable instead of mysterious silence.
func shutdown(srv *http.Server, cfg *config.Server, log *slog.Logger) {
	log.Info("shutdown initiated; draining in-flight requests",
		slog.Duration("max_wait", cfg.ShutdownDrainTimeout))

	ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownDrainTimeout)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- srv.Shutdown(ctx) }()

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
				if closeErr := srv.Close(); closeErr != nil && !errors.Is(closeErr, http.ErrServerClosed) {
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
			log.Info("still draining…", slog.Int64("elapsed_ms", time.Since(start).Milliseconds()))
		}
	}
}
