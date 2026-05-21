package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

func (r *Runtime) Run(ctx context.Context) error {
	if r == nil || r.Server == nil {
		return errors.New("gateway runtime server is required")
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
	r.shutdown() //nolint:contextcheck // shutdown creates a fresh deadline after parent ctx is already done (SIGTERM caught)
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
