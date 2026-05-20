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

	"llmgate/internal/app/gateway"
	"llmgate/internal/catalog"
	"llmgate/internal/config"
	"llmgate/internal/consumers"
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
		slog.String("version", version),
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

	runtime, err := gateway.BuildRuntime(context.Background(), gateway.RuntimeInput{
		Config:    cfg,
		Catalog:   cat,
		Consumers: consumerStore,
		Logger:    logger,
		Version:   version,
	})
	if err != nil {
		return err
	}
	defer func() { _ = runtime.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("server listening", slog.String("addr", cfg.Addr))
		err := runtime.Server.ListenAndServe()
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
	runtime.Probe.MarkShuttingDown()
	shutdown(runtime.Server, cfg, logger)
	if serveErr != nil {
		return serveErr
	}
	return nil
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
