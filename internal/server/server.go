package server

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"

	"llmgate/internal/config"
)

func New(cfg *config.Server, log *slog.Logger, h *Handler) *http.Server {
	r := chi.NewRouter()
	r.Use(requestIDMiddleware)
	r.Use(accessLogMiddleware(log))
	r.Use(chimiddleware.Recoverer)

	// Liveness only; this does not verify upstream availability.
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{\"status\":\"ok\"}"))
	})
	r.Post("/v1/chat/completions", h.ServeHTTP)

	return &http.Server{
		Addr:              cfg.Addr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		WriteTimeout:      0,
	}
}
