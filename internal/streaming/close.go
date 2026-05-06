package streaming

import "time"

// CloseGrace bounds how long a caller waits for a Stream.Recv goroutine to
// exit after Close() before abandoning it. Adapters are required by the
// Stream contract to unblock Recv promptly on Close; this var is the
// defensive ceiling for misbehaving adapters that violate the contract.
// It is a var (not a const) so tests can override it for speed.
var CloseGrace = 5 * time.Second

// DrainRecvOrAbandon waits up to grace for ch to deliver a result, or returns.
// The caller's buffered channel keeps the abandoned Recv goroutine from
// leaking the channel itself; the goroutine is left to complete in the
// background if the adapter never honors Close.
func DrainRecvOrAbandon[T any](ch <-chan T, grace time.Duration) {
	select {
	case <-ch:
	case <-time.After(grace):
	}
}
