package server

import (
	"context"
	"net/http"
	"strings"

	"llmgate/internal/audit"
	"llmgate/internal/consumers"
)

// ConsumerInfo describes the registered caller behind a request, populated
// by the auth middleware on every protected route. Name and KeyID are set
// only when Authorization successfully matched a registered consumer; on
// failure AuthError is non-empty and identifies the failure mode so the
// handler can audit-emit and short-circuit. The middleware *never*
// short-circuits itself — the handler stays the single audit emitter
// (ADR 001 / ADR 003 audit-always).
type ConsumerInfo struct {
	Name      string
	KeyID     string
	AuthError audit.AuthError
}

type consumerCtxKey struct{}

// ConsumerFromContext returns the latest ConsumerInfo placed on ctx. The
// underlying value is a *ConsumerInfo (allocated by consumerContextMiddleware
// at the start of the chain) so that inner middleware mutations are
// visible to outer middleware after the chain unwinds — see the doc on
// consumerContextMiddleware. Returns the zero value if no context
// bootstrap ran (e.g. unit tests calling the handler directly).
func ConsumerFromContext(ctx context.Context) ConsumerInfo {
	if p, ok := ctx.Value(consumerCtxKey{}).(*ConsumerInfo); ok && p != nil {
		return *p
	}
	return ConsumerInfo{}
}

// consumerContextMiddleware allocates a heap *ConsumerInfo and places it on
// ctx so the auth middleware can mutate it via pointer. Without this
// step a value-typed ctx update by the auth middleware would be
// invisible to the outer access log (the outer holds a reference to the
// parent ctx, not the chain-modified one). Run this before any
// middleware that wants to read consumer identity.
func consumerContextMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := &ConsumerInfo{}
		ctx := context.WithValue(r.Context(), consumerCtxKey{}, info)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// authMiddleware builds an HTTP middleware that classifies the
// Authorization header against the given consumers store and writes the
// result through the *ConsumerInfo pointer placed on ctx by
// consumerContextMiddleware. Always calls next — the handler is the
// single audit emitter and must run even on auth failure.
func authMiddleware(store *consumers.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if p, ok := r.Context().Value(consumerCtxKey{}).(*ConsumerInfo); ok && p != nil {
				*p = classifyAuth(r, store)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// classifyAuth inspects the Authorization header and looks the bearer
// token up in store. It deliberately does not log the raw key on any
// failure path — the audit.AuthError is the only signal that escapes.
func classifyAuth(r *http.Request, store *consumers.Store) ConsumerInfo {
	raw := r.Header.Get("Authorization")
	if raw == "" {
		return ConsumerInfo{AuthError: audit.AuthErrorMissing}
	}
	const prefix = "Bearer "
	if len(raw) <= len(prefix) || !strings.EqualFold(raw[:len(prefix)], prefix) {
		return ConsumerInfo{AuthError: audit.AuthErrorFormat}
	}
	key := strings.TrimSpace(raw[len(prefix):])
	if key == "" {
		return ConsumerInfo{AuthError: audit.AuthErrorFormat}
	}
	name, keyID, ok := store.Lookup(key)
	if !ok {
		return ConsumerInfo{AuthError: audit.AuthErrorUnknown}
	}
	return ConsumerInfo{Name: name, KeyID: keyID}
}
