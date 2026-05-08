package server

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"

	"llmgate/internal/config"
	"llmgate/internal/consumers"
)

func New(cfg *config.Server, log *slog.Logger, h *Handler, store *consumers.Store, probe *ProbeState) *http.Server {
	r := chi.NewRouter()
	r.Use(requestIDMiddleware)
	// consumerContextMiddleware must run before accessLogMiddleware so the
	// access log's defer can read whatever the auth middleware later
	// writes through the shared *ConsumerInfo pointer.
	r.Use(consumerContextMiddleware)
	r.Use(accessLogMiddleware(log))
	r.Use(chimiddleware.Recoverer)

	// Probe endpoints: intentionally unauthenticated so orchestrators
	// (k8s probes, load balancers) work without a registered consumer.
	// Two semantics:
	//   /healthz/live  — always 200 (process alive)
	//   /healthz/ready — 503 once SIGTERM has been received so endpoint
	//                    controllers can drop this pod before drain
	// /healthz stays as a backward-compatible alias of /healthz/ready
	// so existing manifests get the better shutdown behavior for free.
	readyHandler := readiness(probe)
	r.Get("/healthz", readyHandler)
	r.Get("/healthz/live", liveness)
	r.Get("/healthz/ready", readyHandler)
	// Auth scope: only the chat endpoint sits behind auth, so probes
	// stay public. Add new auth-required routes inside this Group.
	r.Group(func(r chi.Router) {
		r.Use(authMiddleware(store))
		r.Post("/v1/chat/completions", h.ServeHTTP)
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
