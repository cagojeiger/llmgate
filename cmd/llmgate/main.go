package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

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
	return runtime.Run(ctx)
}
