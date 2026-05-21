package middleware

import (
	"log/slog"
	"net/http"
	"time"

	httpauth "llmgate/internal/platform/http/auth"
	"llmgate/internal/platform/http/requestid"
	"llmgate/internal/platform/http/response"
)

// RequestID propagates an X-Request-Id header end-to-end: it
// validates any inbound value via requestid.Valid (printable ASCII,
// length-bounded so a client can't smuggle log-injection bytes or
// oversize blobs), generates a fresh id otherwise, mirrors the final
// value on both the request and response headers, and stashes it on
// ctx so downstream code (access log, audit emit, llm-result emit)
// resolves the same id via requestid.FromContext.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if !requestid.Valid(id) {
			id = requestid.New()
		}

		w.Header().Set("X-Request-Id", id)
		r.Header.Set("X-Request-Id", id)
		ctx := requestid.WithContext(r.Context(), id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// AccessLog returns the per-request access-log middleware. It must be
// mounted *after* the auth-context middleware so the deferred log
// line can read whatever consumer identity (or auth_error) the auth
// step later wrote through the shared pointer. The auth_error attr
// is the only place an operator can tell apart "no Authorization
// header" / "bad format" / "unknown key" — the wire response just
// returns 401 for all three.
func AccessLog(log *slog.Logger) func(http.Handler) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			cw := response.NewCountingWriter(w)

			next.ServeHTTP(cw, r)

			// Pull consumer identity that the auth middleware (when wired
			// for this route) set on ctx during the pre-handler phase.
			// Empty string for routes without auth (e.g. /healthz) or
			// when auth rejected the request.
			consumer := httpauth.FromContext(r.Context())

			attrs := []slog.Attr{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", cw.Status()),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()),
				slog.Int64("bytes_out", cw.Bytes()),
				slog.String("request_id", requestid.FromContext(r.Context())),
				slog.String("consumer_name", consumer.Name),
			}
			if consumer.AuthError != "" {
				// Surface the auth-failure mode (missing / format / unknown)
				// here too — the wire response only ever shows 401, so
				// without this attr operators couldn't distinguish "no
				// header sent" from "key rotated out" without diving into
				// the audit stream.
				attrs = append(attrs, slog.String("auth_error", string(consumer.AuthError)))
			}
			log.LogAttrs(r.Context(), slog.LevelInfo, "request", attrs...)
		})
	}
}
