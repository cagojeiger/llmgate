package systemone

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	llmresultschema "llmgate/internal/domain/llmresult/schema"
	httpauth "llmgate/internal/platform/http/auth"
)

type unreadableBody struct {
	read bool
}

func (b *unreadableBody) Read([]byte) (int, error) {
	b.read = true
	return 0, errors.New("capacity rejection read the body")
}
func (*unreadableBody) Close() error { return nil }

func TestPayloadCapacityHeldUntilAuditIsRecorded(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, answer)
	}))
	defer upstream.Close()
	recording := make(chan struct{}, 2)
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	h := NewHandler(nil, nil, HandlerConfig{
		APIKey: "key", Endpoint: upstream.URL,
		RecordResult: func(_ context.Context, ev *llmresultschema.Event) error {
			if ev.StatusCode != http.StatusOK || string(ev.DecisionResponse) != answer {
				t.Error("recorder did not receive the completed decision")
			}
			recording <- struct{}{}
			<-release
			return nil
		},
	})
	done := make(chan int, cap(h.payloadSlots))
	for range cap(h.payloadSlots) {
		go func() {
			req := systemOneRequest(inputBody, httpauth.ConsumerInfo{Name: "consumer"})
			req.Header.Set("TypeSafe-Danger-Zone-Images", "true")
			out := httptest.NewRecorder()
			h.ServeHTTP(out, req)
			done <- out.Code
		}()
	}
	for range cap(h.payloadSlots) {
		select {
		case <-recording:
		case <-time.After(3 * time.Second):
			t.Fatal("request did not enter the audit recorder")
		}
	}
	select {
	case <-done:
		t.Fatal("response completed before recording")
	default:
	}
	body := &unreadableBody{}
	req := systemOneRequest(inputBody, httpauth.ConsumerInfo{Name: "consumer"})
	req.Body = body
	req.ContentLength = guardedRequestBytes + 1
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	if out.Code != http.StatusTooManyRequests || out.Header().Get("Retry-After") != "1" || body.read || calls.Load() != int32(cap(h.payloadSlots)) {
		t.Fatalf("saturated request was not rejected before reading: HTTP %d, retry %q, read %v, provider calls %d",
			out.Code, out.Header().Get("Retry-After"), body.read, calls.Load())
	}
	close(release)
	for range cap(h.payloadSlots) {
		if status := <-done; status != http.StatusOK {
			t.Errorf("recorded request returned HTTP %d", status)
		}
	}
}

func TestPayloadAuditFailureWithholdsSuccessfulDecision(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, answer)
	}))
	defer upstream.Close()
	recordAttempts := 0
	events := &eventCapture{}
	results := &resultCapture{}
	h := NewHandler(nil, events, HandlerConfig{
		APIKey: "key", Endpoint: upstream.URL, ResultSink: results,
		RecordResult: func(context.Context, *llmresultschema.Event) error {
			recordAttempts++
			return errors.New("disk write failed")
		},
	})
	req := systemOneRequest(inputBody, httpauth.ConsumerInfo{Name: "consumer"})
	req.Header.Set("TypeSafe-Danger-Zone-Images", "true")
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	if out.Code != http.StatusServiceUnavailable || strings.Contains(out.Body.String(), "answers") {
		t.Fatalf("audit write failure returned HTTP %d: %s", out.Code, out.Body.String())
	}
	if recordAttempts != 1 || len(results.events) != 0 || len(events.events) != 2 {
		t.Fatalf("unexpected event counts: %d direct attempts, %d async results, %d telemetry", recordAttempts, len(results.events), len(events.events))
	}
}
