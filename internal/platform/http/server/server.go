package server

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"

	"llmgate/internal/domain/consumers"
	"llmgate/internal/platform/config"
	httpauth "llmgate/internal/platform/http/auth"
	httpmiddleware "llmgate/internal/platform/http/middleware"
	httpprobe "llmgate/internal/platform/http/probe"
)

type ServerOptions struct {
	Config         *config.Server
	Log            *slog.Logger
	Handler        http.Handler
	Consumers      *consumers.Store
	Probe          *httpprobe.State
	MetricsHandler http.Handler
}

func New(cfg *config.Server, log *slog.Logger, h http.Handler, store *consumers.Store, probe *httpprobe.State) *http.Server {
	return NewWithOptions(ServerOptions{
		Config:    cfg,
		Log:       log,
		Handler:   h,
		Consumers: store,
		Probe:     probe,
	})
}

func NewWithOptions(opts ServerOptions) *http.Server {
	cfg := opts.Config
	log := opts.Log
	h := opts.Handler
	store := opts.Consumers
	probe := opts.Probe

	r := chi.NewRouter()

	// Probes sit *outside* the middleware chain. k8s liveness / readiness
	// fire every few seconds and would otherwise dominate the access
	// stream; the sidecar (Istio/Envoy) already records every inbound
	// request, so dropping probes from the app-level access log costs
	// no debugging visibility. Probe handlers are an atomic.Bool read +
	// a Write — Recoverer is unnecessary, and skipping requestID /
	// consumerContext keeps probe spans out of any tracing backend.
	// Two semantics:
	//   /healthz/live  — always 200 (process alive)
	//   /healthz/ready — 503 once SIGTERM has been received so endpoint
	//                    controllers can drop this pod before drain
	// /healthz stays as a backward-compatible alias of /healthz/ready
	// so existing manifests get the better shutdown behavior for free.
	readyHandler := httpprobe.Ready(probe)
	r.Get("/healthz", readyHandler)
	r.Get("/healthz/live", httpprobe.Live)
	r.Get("/healthz/ready", readyHandler)
	if opts.MetricsHandler != nil {
		r.Handle("/metrics", opts.MetricsHandler)
	}

	// Business traffic carries the full middleware chain. Probes are
	// already mounted above, and /metrics joins that public scrape surface
	// when configured, so nothing in this group ever sees them.
	r.Group(func(r chi.Router) {
		r.Use(httpmiddleware.RequestID)
		// auth.ContextMiddleware must run before middleware.AccessLog so the
		// access log's defer can read whatever the auth middleware later
		// writes through the shared *httpauth.ConsumerInfo pointer.
		r.Use(httpauth.ContextMiddleware)
		r.Use(httpmiddleware.AccessLog(log))
		r.Use(chimiddleware.Recoverer)

		// Auth scope: only chat sits behind auth today. Future
		// auth-required routes go inside this inner Group.
		r.Group(func(r chi.Router) {
			r.Use(httpauth.Middleware(store))
			r.Post("/v1/chat/completions", h.ServeHTTP)
		})
	})

	return &http.Server{
		Addr:              cfg.Addr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.RequestTimeout,
		IdleTimeout:       60 * time.Second,
		WriteTimeout:      0,
	}
}
