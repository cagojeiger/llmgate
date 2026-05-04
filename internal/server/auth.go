package server

import (
	"context"
	"net/http"
	"strings"

	"llmgate/internal/clients"
)

// ClientInfo describes the registered caller behind a request, populated
// by the auth middleware on every protected route. Name and KeyID are set
// only when Authorization successfully matched a registered client; on
// failure AuthError is non-empty and identifies the failure mode so the
// handler can audit-emit and short-circuit. The middleware *never*
// short-circuits itself — the handler stays the single audit emitter
// (ADR 007 / ADR 008 audit-always).
type ClientInfo struct {
	Name      string
	KeyID     string
	AuthError AuthErrorKind
}

// AuthErrorKind is the reason auth failed at the gateway boundary. The
// handler maps every non-empty value to a 401 KindAuth response; the
// distinction is forwarded to audit Record.AuthError + the access log's
// auth_error attr so operators can grep "missing header" vs "unknown
// key" without re-reading the matching code.
type AuthErrorKind string

const (
	AuthErrorMissing AuthErrorKind = "missing"
	AuthErrorFormat  AuthErrorKind = "format"
	AuthErrorUnknown AuthErrorKind = "unknown"
)

type clientCtxKey struct{}

// ClientFromContext returns the latest ClientInfo placed on ctx. The
// underlying value is a *ClientInfo (allocated by clientContextMiddleware
// at the start of the chain) so that inner middleware mutations are
// visible to outer middleware after the chain unwinds — see the doc on
// clientContextMiddleware. Returns the zero value if no context
// bootstrap ran (e.g. unit tests calling the handler directly).
func ClientFromContext(ctx context.Context) ClientInfo {
	if p, ok := ctx.Value(clientCtxKey{}).(*ClientInfo); ok && p != nil {
		return *p
	}
	return ClientInfo{}
}

// clientContextMiddleware allocates a heap *ClientInfo and places it on
// ctx so the auth middleware can mutate it via pointer. Without this
// step a value-typed ctx update by the auth middleware would be
// invisible to the outer access log (the outer holds a reference to the
// parent ctx, not the chain-modified one). Run this before any
// middleware that wants to read client identity.
func clientContextMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := &ClientInfo{}
		ctx := context.WithValue(r.Context(), clientCtxKey{}, info)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// authMiddleware builds an HTTP middleware that classifies the
// Authorization header against the given clients store and writes the
// result through the *ClientInfo pointer placed on ctx by
// clientContextMiddleware. Always calls next — the handler is the
// single audit emitter and must run even on auth failure.
func authMiddleware(store *clients.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if p, ok := r.Context().Value(clientCtxKey{}).(*ClientInfo); ok && p != nil {
				*p = classifyAuth(r, store)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// classifyAuth inspects the Authorization header and looks the bearer
// token up in store. It deliberately does not log the raw key on any
// failure path — the AuthErrorKind is the only signal that escapes.
func classifyAuth(r *http.Request, store *clients.Store) ClientInfo {
	raw := r.Header.Get("Authorization")
	if raw == "" {
		return ClientInfo{AuthError: AuthErrorMissing}
	}
	const prefix = "Bearer "
	if len(raw) <= len(prefix) || !strings.EqualFold(raw[:len(prefix)], prefix) {
		return ClientInfo{AuthError: AuthErrorFormat}
	}
	key := strings.TrimSpace(raw[len(prefix):])
	if key == "" {
		return ClientInfo{AuthError: AuthErrorFormat}
	}
	name, keyID, ok := store.Lookup(key)
	if !ok {
		return ClientInfo{AuthError: AuthErrorUnknown}
	}
	return ClientInfo{Name: name, KeyID: keyID}
}
