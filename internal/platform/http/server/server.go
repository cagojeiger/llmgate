package server

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

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
		r.Handle("/metrics", metricsAuth(opts.MetricsHandler, cfg.MetricsBearerToken))
	}

	// Business traffic carries the full middleware chain. Order is
	// encoded inside StandardChain.Apply so it cannot drift by
	// re-arranging r.Use calls at this site. Probes are already
	// mounted above, and /metrics joins that public scrape surface
	// when configured, so nothing in this group ever sees them.
	chain := httpmiddleware.NewStandardChain(log)
	r.Group(func(r chi.Router) {
		chain.Apply(r)

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

func metricsAuth(next http.Handler, token string) http.Handler {
	if token == "" {
		return next
	}
	want := "Bearer " + token
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimSpace(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="llmgate-metrics"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
