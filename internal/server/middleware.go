package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

type requestIDCtxKey struct{}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if !validRequestID(id) {
			id = newRequestID()
		}

		w.Header().Set("X-Request-Id", id)
		r.Header.Set("X-Request-Id", id)
		ctx := context.WithValue(r.Context(), requestIDCtxKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDCtxKey{}).(string)
	return id
}

func validRequestID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x20 || id[i] > 0x7e {
			return false
		}
	}
	return true
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	n := time.Now().UnixNano()
	if n == 0 {
		n = 1
	}
	return fmt.Sprintf("ts-%016x", n)
}

func accessLogMiddleware(log *slog.Logger) func(http.Handler) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			cw := &countingWriter{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(cw, r)

			// Pull consumer identity that the auth middleware (when wired
			// for this route) set on ctx during the pre-handler phase.
			// Empty string for routes without auth (e.g. /healthz) or
			// when auth rejected the request.
			consumer := ConsumerFromContext(r.Context())

			attrs := []slog.Attr{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", cw.status),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()),
				slog.Int64("bytes_out", cw.bytes),
				slog.String("request_id", RequestIDFromContext(r.Context())),
				slog.String("consumer", consumer.Name),
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

type countingWriter struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (w *countingWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *countingWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *countingWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *countingWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
