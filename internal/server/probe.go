package server

import (
	"net/http"
	"sync/atomic"
)

// ProbeState backs the k8s liveness / readiness probes. The cmd/llmgate
// process owns one instance and flips it via MarkShuttingDown when
// SIGTERM arrives so /healthz/ready starts returning 503 *before* the
// drain phase begins. The endpoint controller can then remove this pod
// from the service while in-flight requests still get to finish.
//
// The struct exists (rather than a bare *atomic.Bool) because the probe
// surface is the natural place to add additional bits later — e.g. a
// boot-complete flag if catalog loading ever moves behind the listener.
type ProbeState struct {
	shuttingDown atomic.Bool
}

// NewProbeState returns a ProbeState whose flags are all unset.
func NewProbeState() *ProbeState { return &ProbeState{} }

// MarkShuttingDown flips the shutting-down bit. Idempotent.
func (p *ProbeState) MarkShuttingDown() {
	if p == nil {
		return
	}
	p.shuttingDown.Store(true)
}

// IsShuttingDown reports whether MarkShuttingDown has been called.
func (p *ProbeState) IsShuttingDown() bool {
	if p == nil {
		return false
	}
	return p.shuttingDown.Load()
}

// liveness always returns 200 — the process responding at all is the
// liveness signal. k8s should only restart this pod when the response
// itself stops (deadlock / hang), not when shutdown is in progress.
func liveness(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// readiness returns 503 once MarkShuttingDown has been called so the
// k8s endpoint controller drops this pod from the service before the
// drain phase starts. While the pod is healthy and not shutting down it
// returns 200. The body is deliberately tiny so probe responses fit in
// one packet.
func readiness(state *ProbeState) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if state.IsShuttingDown() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"shutting_down"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	}
}
