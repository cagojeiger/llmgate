package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"llmgate/internal/config"
)

func newTestServer(t *testing.T, probe *ProbeState) (*httptest.Server, func()) {
	t.Helper()
	store := writeStoreYAML(t, "alpha", "good-key")
	handler := NewHandler(&stubGateway{}, slog.Default(), &recordingRecorder{}, HandlerConfig{})
	srv := New(&config.Server{Addr: ":0"}, slog.Default(), handler, store, probe)
	ts := httptest.NewServer(srv.Handler)
	return ts, ts.Close
}

func probeStatus(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("Get %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	status, _ := got["status"].(string)
	return resp.StatusCode, status
}

func TestProbe_LiveAlwaysOK(t *testing.T) {
	probe := NewProbeState()
	ts, cleanup := newTestServer(t, probe)
	defer cleanup()

	// Healthy
	if code, status := probeStatus(t, ts.URL+"/healthz/live"); code != 200 || status != "ok" {
		t.Fatalf("healthy: code=%d status=%q, want 200 ok", code, status)
	}

	// Liveness must stay 200 even during shutdown — the process is
	// still alive (responding); k8s suppresses restart on Terminating
	// pods anyway, but having liveness flap to 503 here would hide
	// real liveness deadlocks downstream.
	probe.MarkShuttingDown()
	if code, status := probeStatus(t, ts.URL+"/healthz/live"); code != 200 || status != "ok" {
		t.Fatalf("during shutdown: code=%d status=%q, want 200 ok (liveness must not flap)", code, status)
	}
}

func TestProbe_ReadyFlipsOnShutdown(t *testing.T) {
	probe := NewProbeState()
	ts, cleanup := newTestServer(t, probe)
	defer cleanup()

	if code, status := probeStatus(t, ts.URL+"/healthz/ready"); code != 200 || status != "ready" {
		t.Fatalf("healthy: code=%d status=%q, want 200 ready", code, status)
	}

	probe.MarkShuttingDown()
	if code, status := probeStatus(t, ts.URL+"/healthz/ready"); code != 503 || status != "shutting_down" {
		t.Fatalf("after MarkShuttingDown: code=%d status=%q, want 503 shutting_down", code, status)
	}
}

func TestProbe_HealthzAliasMatchesReady(t *testing.T) {
	// /healthz must mirror /healthz/ready so existing manifests get the
	// shutdown-aware behavior without an explicit migration. This is the
	// backward-compatibility contract.
	probe := NewProbeState()
	ts, cleanup := newTestServer(t, probe)
	defer cleanup()

	if code, _ := probeStatus(t, ts.URL+"/healthz"); code != 200 {
		t.Fatalf("/healthz before shutdown = %d, want 200", code)
	}
	probe.MarkShuttingDown()
	if code, _ := probeStatus(t, ts.URL+"/healthz"); code != 503 {
		t.Fatalf("/healthz after MarkShuttingDown = %d, want 503", code)
	}
}

func TestProbe_PublicWithoutAuth(t *testing.T) {
	probe := NewProbeState()
	ts, cleanup := newTestServer(t, probe)
	defer cleanup()

	for _, path := range []string{"/healthz", "/healthz/live", "/healthz/ready"} {
		// No Authorization header — must still get a non-401 response
		// because probe routes sit outside the auth Group.
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("Get %s: %v", path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			t.Errorf("%s returned 401; probe routes must be public", path)
		}
	}
}

func TestProbe_NilStateTreatedAsHealthy(t *testing.T) {
	// server.New tolerates a nil ProbeState (used by some unit tests
	// that don't care about probes). When state is nil the readiness
	// handler must still return a sane 200 instead of panicking.
	ts, cleanup := newTestServer(t, nil)
	defer cleanup()

	if code, status := probeStatus(t, ts.URL+"/healthz/ready"); code != 200 || status != "ready" {
		t.Fatalf("nil state: code=%d status=%q, want 200 ready", code, status)
	}
}

func TestProbeState_Idempotent(t *testing.T) {
	probe := NewProbeState()
	if probe.IsShuttingDown() {
		t.Fatal("zero state must report not shutting down")
	}
	probe.MarkShuttingDown()
	probe.MarkShuttingDown() // idempotent — no panic, no flip back
	if !probe.IsShuttingDown() {
		t.Fatal("after MarkShuttingDown must report shutting down")
	}
}

func TestProbeState_NilSafe(t *testing.T) {
	var probe *ProbeState
	probe.MarkShuttingDown() // no panic
	if probe.IsShuttingDown() {
		t.Fatal("nil pointer must read as not shutting down")
	}
}
