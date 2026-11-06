package systemone

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/telemetry"
	httpauth "llmgate/internal/platform/http/auth"
)

type delayedRequestBody struct {
	io.Reader
	delay time.Duration
}

func (r *delayedRequestBody) Read(p []byte) (int, error) {
	if r.delay > 0 {
		time.Sleep(r.delay)
		r.delay = 0
	}
	return r.Reader.Read(p)
}

type timedOutRequestBody struct{ cancel context.CancelFunc }

func (r timedOutRequestBody) Read([]byte) (int, error) {
	r.cancel()
	return 0, &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
}

type timeoutTestTransport func(*http.Request) (*http.Response, error)

func (f timeoutTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestSystemOneStartsCompleteTimeoutAfterReadingRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const completeTimeout = time.Second
		h := NewHandler(nil, nil, HandlerConfig{
			APIKey: "key", RequestTimeout: 5 * time.Second, CompleteTimeout: completeTimeout,
		})
		h.client.Transport = timeoutTestTransport(func(r *http.Request) (*http.Response, error) {
			deadline, ok := r.Context().Deadline()
			remaining := time.Until(deadline)
			if !ok || remaining != completeTimeout {
				t.Fatalf("upstream attempt deadline remaining = %s, want %s", remaining, completeTimeout)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(answer))}, nil
		})

		r := systemOneRequest(inputBody, httpauth.ConsumerInfo{Name: "consumer"})
		r.Body = io.NopCloser(&delayedRequestBody{Reader: r.Body, delay: 2 * completeTimeout})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK || w.Body.String() != answer {
			t.Fatalf("got %d %s, want successful response after slow upload", w.Code, w.Body)
		}
	})
}

func TestSystemOneRequestTimeoutBeforeAttemptOnlyAudits(t *testing.T) {
	tests := []struct {
		name string
		body func(io.Reader, context.CancelFunc) io.Reader
	}{
		{"request deadline", func(body io.Reader, _ context.CancelFunc) io.Reader {
			return &delayedRequestBody{Reader: body, delay: 2 * time.Second}
		}},
		{"socket deadline with canceled request context", func(_ io.Reader, cancel context.CancelFunc) io.Reader {
			return timedOutRequestBody{cancel: cancel}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				events, results := &eventCapture{}, &resultCapture{}
				h := NewHandler(nil, events, HandlerConfig{
					APIKey: "key", RequestTimeout: time.Second, ResultSink: results,
				})
				h.client.Transport = timeoutTestTransport(func(*http.Request) (*http.Response, error) {
					t.Fatal("the expired request reached the upstream transport")
					return nil, nil
				})

				r := systemOneRequest(inputBody, httpauth.ConsumerInfo{Name: "consumer"})
				ctx, cancel := context.WithCancel(r.Context())
				defer cancel()
				r.Body = io.NopCloser(tc.body(r.Body, cancel))
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r.WithContext(ctx))
				if w.Code != http.StatusBadGateway || len(events.events) != 1 || len(results.events) != 0 {
					t.Fatalf("got status %d, %d telemetry and %d result events, want 502 and audit only", w.Code, len(events.events), len(results.events))
				}
				if audit := events.events[0].(*telemetry.AuditEvent); audit.Kind != llmtypes.KindTimeout {
					t.Fatalf("got audit kind %q, want timeout", audit.Kind)
				}
			})
		})
	}
}

type blockedProviderBody struct{ ctx context.Context }

func (r blockedProviderBody) Read([]byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func TestSystemOneCompleteTimeoutCoversProviderBody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const completeTimeout = time.Second
		events, results := &eventCapture{}, &resultCapture{}
		h := NewHandler(nil, events, HandlerConfig{
			APIKey: "key", RequestTimeout: 5 * time.Second, CompleteTimeout: completeTimeout, ResultSink: results,
		})
		h.client.Transport = timeoutTestTransport(func(r *http.Request) (*http.Response, error) {
			body := io.NopCloser(blockedProviderBody{ctx: r.Context()})
			return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
		})

		r := systemOneRequest(inputBody, httpauth.ConsumerInfo{Name: "consumer"})
		w := httptest.NewRecorder()
		start := time.Now()
		h.ServeHTTP(w, r)
		if elapsed := time.Since(start); elapsed != completeTimeout {
			t.Fatalf("blocked upstream body read stopped after %s, want %s", elapsed, completeTimeout)
		}
		if w.Code != http.StatusBadGateway || len(events.events) != 2 || len(results.events) != 1 {
			t.Fatalf("got status %d, %d telemetry and %d result events, want 502 and one attempted call", w.Code, len(events.events), len(results.events))
		}
		result := results.events[0]
		if result.ErrorKind != llmtypes.KindTimeout || len(result.DecisionResponse) != 0 || len(result.Attempts) != 1 || result.Attempts[0].Kind != llmtypes.KindTimeout {
			t.Fatalf("unexpected result after upstream timeout: %+v", result)
		}
	})
}
