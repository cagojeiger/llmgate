package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"

	"llmgate/internal/config"
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

	fwd, err := server.NewForwarder(&cfg.Provider, logger)
	if err != nil {
		return err
	}
	srv := server.New(cfg, logger, fwd)

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
