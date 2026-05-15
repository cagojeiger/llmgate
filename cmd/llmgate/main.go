package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"llmgate/internal/audit"
	"llmgate/internal/catalog"
	"llmgate/internal/config"
	"llmgate/internal/consumers"
	"llmgate/internal/llmrouter"
	"llmgate/internal/llmtypes"
	"llmgate/internal/providers/anthropic"
	"llmgate/internal/providers/openai"
	"llmgate/internal/server"
)

// version is set by the linker at build time via
// `-ldflags "-X main.version=<vX.Y.Z>"`. Defaults to "dev" for plain
// `go run` / `go build` so unset binaries are easy to spot.
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "-v":
			fmt.Println("llmgate", version)
			return
		}
	}
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
	accessLog := logger.With(slog.String("log", "access"))
	auditLog := logger.With(slog.String("log", "audit"))
	callLog := logger.With(slog.String("log", "call"))

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

	factories := map[llmtypes.Protocol]providerFactory{
		llmtypes.ProtocolOpenAI:    openaiFactory,
		llmtypes.ProtocolAnthropic: anthropicFactory,
	}
	models, aliases, err := buildRouterInputs(cat, factories)
	if err != nil {
		return err
	}

	policy := llmrouter.FallbackPolicy{
		OnKinds:         cfg.FallbackOn,
		CircuitFailures: cfg.CircuitFailures,
		CircuitOpen:     cfg.CircuitOpen,
		CircuitMaxOpen:  cfg.CircuitMaxOpen,
		CircuitJitter:   cfg.CircuitJitter,
		CompleteTimeout: cfg.CompleteTimeout,
	}
	svc, err := llmrouter.NewService(models, aliases, policy, logger)
	if err != nil {
		return err
	}

	recorder := audit.Recorders{audit.NewSlogRecorder(auditLog)}
	callRecorder := audit.CallRecorders{audit.NewSlogCallRecorder(callLog)}
	defer func() {
		if err := recorder.Close(); err != nil {
			logger.Warn("audit recorder close failed", slog.String("err", err.Error()))
		}
		if err := callRecorder.Close(); err != nil {
			logger.Warn("call recorder close failed", slog.String("err", err.Error()))
		}
	}()

	handler := server.NewHandler(svc, logger, recorder, callRecorder, server.HandlerConfig{
		RequestTimeout:    cfg.RequestTimeout,
		StreamIdleTimeout: cfg.StreamIdleTimeout,
	})
	probe := server.NewProbeState()
	srv := server.New(cfg, accessLog, handler, consumerStore, probe)

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
// the cmd binary because it bridges two boundaries the llmrouter package
// deliberately does not know about: the catalog yaml shape (catalog.Model
// fields) and the env-driven credential lookup (auth_env).
type providerFactory func(*catalog.Model) (llmtypes.Provider, error)

// buildRouterInputs walks the catalog and turns it into the runtime
// shape the Service expects: model id → already-instantiated Provider,
// alias name → ordered chain of model ids. The Service itself stays
// catalog-agnostic; this helper is the single point that bridges the
// yaml shape into the service.
func buildRouterInputs(cat *catalog.Catalog, factories map[llmtypes.Protocol]providerFactory) (llmrouter.Models, llmrouter.Aliases, error) {
	models := make(llmrouter.Models, len(cat.Models))
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
	aliases := make(llmrouter.Aliases, len(cat.Aliases))
	for name, a := range cat.Aliases {
		aliases[name] = append([]string(nil), a.Chain...)
	}
	return models, aliases, nil
}

func openaiFactory(m *catalog.Model) (llmtypes.Provider, error) {
	apiKey, err := readAuthKey(m)
	if err != nil {
		var missing *missingAuthKeyError
		if errors.As(err, &missing) {
			return missingAuthProviderFor(m, missing.Env), nil
		}
		return nil, err
	}
	return openai.New(openai.Config{
		BaseURL:    m.BaseURL,
		APIKey:     apiKey,
		AuthScheme: m.AuthScheme,
		Name:       m.Vendor,
		ExtraBody:  m.ExtraBody,
	})
}

func anthropicFactory(m *catalog.Model) (llmtypes.Provider, error) {
	apiKey, err := readAuthKey(m)
	if err != nil {
		var missing *missingAuthKeyError
		if errors.As(err, &missing) {
			return missingAuthProviderFor(m, missing.Env), nil
		}
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
// When auth_env is omitted in yaml, it defaults to LLMGATE_<VENDOR>_API_KEY
// (vendor uppercased) so most catalog files do not need to repeat the env
// var name. An explicit auth_env always wins.
func readAuthKey(m *catalog.Model) (string, error) {
	envKey := m.AuthEnv
	if envKey == "" {
		envKey = "LLMGATE_" + strings.ToUpper(m.Vendor) + "_API_KEY"
	}
	v := os.Getenv(envKey)
	if v == "" {
		return "", &missingAuthKeyError{Model: m.ID, Env: envKey}
	}
	return v, nil
}

type missingAuthKeyError struct {
	Model string
	Env   string
}

func (e *missingAuthKeyError) Error() string {
	return fmt.Sprintf("model %q: env %s is unset", e.Model, e.Env)
}

type missingAuthProvider struct {
	name  string
	model string
	env   string
}

func missingAuthProviderFor(m *catalog.Model, env string) llmtypes.Provider {
	return &missingAuthProvider{name: m.Vendor, model: m.ID, env: env}
}

func (p *missingAuthProvider) Name() string { return p.name }

func (p *missingAuthProvider) Complete(context.Context, *llmtypes.Request) (*llmtypes.Response, error) {
	return nil, p.err()
}

func (p *missingAuthProvider) CompleteStream(context.Context, *llmtypes.Request) (llmtypes.Stream, error) {
	return nil, p.err()
}

func (p *missingAuthProvider) err() error {
	return &llmtypes.Error{
		Kind:     llmtypes.KindAuth,
		Provider: p.name,
		Message:  fmt.Sprintf("model %q is unavailable because env %s is unset", p.model, p.env),
	}
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
