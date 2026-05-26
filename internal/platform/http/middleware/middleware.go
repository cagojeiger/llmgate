package middleware

import (
	"log/slog"
	"net/http"
	"time"

	httpauth "llmgate/internal/platform/http/auth"
	"llmgate/internal/platform/http/requestid"
	"llmgate/internal/platform/http/response"
)

func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if !requestid.Valid(id) {
			id = requestid.MustNew()
		}

		w.Header().Set("X-Request-Id", id)
		r.Header.Set("X-Request-Id", id)
		ctx := requestid.WithContext(r.Context(), id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

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
				slog.String("remote_addr", r.RemoteAddr),
				slog.String("user_agent", r.UserAgent()),
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
