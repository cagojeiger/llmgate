package auth

import (
	"context"
	"net/http"
	"strings"

	"llmgate/internal/domain/consumers"
	"llmgate/internal/domain/telemetry"
)

// ConsumerInfo describes the registered caller behind a request, populated
// by the auth middleware on every protected route. Name and KeyID are set
// only when Authorization successfully matched a registered consumer; on
// failure AuthError is non-empty and identifies the failure mode. The
// handler emits the audit event and short-circuits. The middleware *never*
// short-circuits itself — the handler stays the single audit emitter
// (ADR 001 / ADR 003 audit-always).
type ConsumerInfo struct {
	Name           string
	KeyID          string
	AllowedAliases []string
	AuthError      telemetry.AuthError
}

type consumerCtxKey struct{}

// FromContext returns the latest ConsumerInfo placed on ctx. The
// underlying value is a *ConsumerInfo (allocated by ContextMiddleware
// at the start of the chain) so that inner middleware mutations are
// visible to outer middleware after the chain unwinds — see the doc on
// ContextMiddleware. Returns the zero value if no context
// bootstrap ran (e.g. unit tests calling the handler directly).
func FromContext(ctx context.Context) ConsumerInfo {
	if p, ok := ctx.Value(consumerCtxKey{}).(*ConsumerInfo); ok && p != nil {
		return *p
	}
	return ConsumerInfo{}
}

// WithConsumer returns a context carrying consumer info. Tests use this to
// exercise handlers without building the full middleware chain.
func WithConsumer(ctx context.Context, info *ConsumerInfo) context.Context {
	return context.WithValue(ctx, consumerCtxKey{}, info)
}

// ContextMiddleware allocates a heap *ConsumerInfo and places it on
// ctx so the auth middleware can mutate it via pointer. Without this
// step a value-typed ctx update by the auth middleware would be
// invisible to the outer access log (the outer holds a reference to the
// parent ctx, not the chain-modified one). Run this before any
// middleware that wants to read consumer identity.
func ContextMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := &ConsumerInfo{}
		ctx := context.WithValue(r.Context(), consumerCtxKey{}, info)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Middleware builds an HTTP middleware that classifies the
// Authorization header against the given consumers store and writes the
// result through the *ConsumerInfo pointer placed on ctx by
// ContextMiddleware. Always calls next — the handler is the
// single audit emitter and must run even on auth failure.
func Middleware(store *consumers.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if p, ok := r.Context().Value(consumerCtxKey{}).(*ConsumerInfo); ok && p != nil {
				*p = Classify(r, store)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Classify inspects the Authorization header and looks the bearer
// token up in store. It deliberately does not log the raw key on any
// failure path — AuthError is the only signal that escapes.
//
// A nil store is treated as a closed allowlist (every key unknown). This
// keeps tests that invoke handlers without wiring a Store, or future
// route configurations that drop the consumers loader, from panicking on
// the lookup.
func Classify(r *http.Request, store *consumers.Store) ConsumerInfo {
	raw := r.Header.Get("Authorization")
	if raw == "" {
		return ConsumerInfo{AuthError: telemetry.AuthErrorMissing}
	}
	const prefix = "Bearer "
	if len(raw) <= len(prefix) || !strings.EqualFold(raw[:len(prefix)], prefix) {
		return ConsumerInfo{AuthError: telemetry.AuthErrorFormat}
	}
	key := strings.TrimSpace(raw[len(prefix):])
	if key == "" {
		return ConsumerInfo{AuthError: telemetry.AuthErrorFormat}
	}
	if store == nil {
		return ConsumerInfo{AuthError: telemetry.AuthErrorUnknown}
	}
	info, ok := store.LookupInfo(key)
	if !ok {
		return ConsumerInfo{AuthError: telemetry.AuthErrorUnknown}
	}
	return ConsumerInfo{Name: info.Name, KeyID: info.KeyID, AllowedAliases: info.AllowedAliases}
}
