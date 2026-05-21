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
	"llmgate/internal/platform/config"
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
	runtime, err := gateway.LoadRuntime(context.Background(), gateway.LoadInput{
		Config:  cfg,
		Logger:  logger,
		Version: version,
	})
	if err != nil {
		return err
	}
	defer func() { _ = runtime.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runtime.Run(ctx)
}
