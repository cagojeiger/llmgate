package server

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"

	"llmgate/internal/clients"
	"llmgate/internal/config"
)

func New(cfg *config.Server, log *slog.Logger, h *Handler, store *clients.Store) *http.Server {
	r := chi.NewRouter()
	r.Use(requestIDMiddleware)
	// clientContextMiddleware must run before accessLogMiddleware so the
	// access log's defer can read whatever the auth middleware later
	// writes through the shared *ClientInfo pointer.
	r.Use(clientContextMiddleware)
	r.Use(accessLogMiddleware(log))
	r.Use(chimiddleware.Recoverer)

	// Liveness only; intentionally unauthenticated so probes from
	// orchestrators (k8s, load balancers) work without a registered
	// client. This does not verify upstream availability.
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{\"status\":\"ok\"}"))
	})
	// Auth scope: only the chat endpoint sits behind auth, so /healthz
	// stays public. Add new auth-required routes inside this Group.
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
