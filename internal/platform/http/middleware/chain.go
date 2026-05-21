package middleware

import (
	"log/slog"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"

	httpauth "llmgate/internal/platform/http/auth"
)

// StandardChain is the canonical middleware stack for business
// routes: request-id → auth-context → access-log → recoverer. It
// exists as a type so the *order* is owned by Apply rather than by
// each caller's r.Use() calls — re-ordering would mean editing this
// method on purpose, not slipping past code review by rearranging a
// few lines at a wire-up site.
type StandardChain struct {
	log *slog.Logger
}

// NewStandardChain returns a chain that will log via log. A nil log
// falls back to slog.Default() so the chain is safe to install in
// unit tests that don't bother wiring a logger.
func NewStandardChain(log *slog.Logger) *StandardChain {
	if log == nil {
		log = slog.Default()
	}
	return &StandardChain{log: log}
}

// Apply installs the standard chain on r in the only valid order.
//
// The ordering invariants live here rather than in caller comments:
//
//   - RequestID first so every downstream middleware can correlate
//     log lines via X-Request-Id.
//   - auth.ContextMiddleware before AccessLog because the access
//     log's deferred line reads the consumer identity (and the
//     auth_error mode that distinguishes "no header" / "bad format"
//     / "unknown key" — the wire response only shows 401) that the
//     auth step writes through the shared pointer.
//   - Recoverer last so it sees any panic from the business handler
//     but does not swallow panics in upstream middleware (which
//     would mask wiring bugs).
func (c *StandardChain) Apply(r chi.Router) {
	r.Use(RequestID)
	r.Use(httpauth.ContextMiddleware)
	r.Use(AccessLog(c.log))
	r.Use(chimiddleware.Recoverer)
}
