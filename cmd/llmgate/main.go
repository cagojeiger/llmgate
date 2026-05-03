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

	"github.com/joho/godotenv"

	"llmgate/internal/audit"
	"llmgate/internal/catalog"
	"llmgate/internal/config"
	"llmgate/internal/provider"
	"llmgate/internal/provider/anthropic"
	"llmgate/internal/provider/openai"
	"llmgate/internal/router"
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

	factories := map[string]router.AdapterFactory{
		"openai":    openaiFactory,
		"anthropic": anthropicFactory,
	}

	policy := router.FallbackPolicy{
		OnKinds:            cfg.FallbackOn,
		CircuitFailures:    cfg.CircuitFailures,
		CircuitOpen:        cfg.CircuitOpen,
		CircuitMaxOpen:     cfg.CircuitMaxOpen,
		CircuitJitter:      cfg.CircuitJitter,
		RequestTimeout:     cfg.RequestTimeout,
		CompleteTimeout:    cfg.CompleteTimeout,
		StreamStartTimeout: cfg.StreamStartTimeout,
	}
	rtr, err := router.NewRouter(cat, factories, policy, logger)
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
	srv := server.New(cfg, logger, handler)

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

	shutdown(srv, cfg, logger)
	if serveErr != nil {
		return serveErr
	}
	return nil
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

func shutdown(srv *http.Server, cfg *config.Server, log *slog.Logger) {
	headerCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownHeaderTimeout)
	err := srv.Shutdown(headerCtx)
	cancel()
	logShutdownError(log, "shutdown header phase failed", err)

	drainCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownDrainTimeout)
	err = srv.Shutdown(drainCtx)
	cancel()
	if err != nil {
		logShutdownError(log, "shutdown drain phase failed", err)
		if errors.Is(err, context.DeadlineExceeded) {
			if closeErr := srv.Close(); closeErr != nil && !errors.Is(closeErr, http.ErrServerClosed) {
				log.Warn("force close failed", slog.String("err", closeErr.Error()))
			}
		}
	}

	log.Info("shutdown complete")
}

func logShutdownError(log *slog.Logger, msg string, err error) {
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Warn(msg, slog.String("err", err.Error()))
	}
}
