package routing

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// fakeClock returns the value set via Set; tests use it to pin
// breakerStore's now() function so cooldown assertions can compare
// exact durations against a known base instead of "current ± 1s".
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock {
	return &fakeClock{t: t}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestBreakerStore(t *testing.T, failureTrip int, base, max time.Duration, jitter float64) (*breakerStore, *fakeClock) {
	t.Helper()
	clock := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	s := newBreakerStore(failureTrip, base, max, jitter, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.now = clock.Now
	return s, clock
}

func TestBreakerStore_BackoffIncreasesAndCaps(t *testing.T) {
	s, clock := newTestBreakerStore(t, 1, 30*time.Second, 2*time.Minute, 0)

	s.recordFailure("deepseek-v4-pro")
	if got := s.states["deepseek-v4-pro"].openUntil; !got.Equal(clock.Now().Add(30 * time.Second)) {
		t.Fatalf("first openUntil = %v, want now+30s", got)
	}

	s.recordFailure("deepseek-v4-pro")
	if got := s.states["deepseek-v4-pro"].openUntil; !got.Equal(clock.Now().Add(60 * time.Second)) {
		t.Fatalf("second openUntil = %v, want now+60s", got)
	}

	s.recordFailure("deepseek-v4-pro")
	if got := s.states["deepseek-v4-pro"].openUntil; !got.Equal(clock.Now().Add(2 * time.Minute)) {
		t.Fatalf("third openUntil = %v, want now+2m (capped)", got)
	}
}

func TestBreakerStore_JitterStaysInRange(t *testing.T) {
	s, clock := newTestBreakerStore(t, 1, 100*time.Second, 5*time.Minute, 0.2)

	s.recordFailure("deepseek-v4-pro")
	got := s.states["deepseek-v4-pro"].openUntil.Sub(clock.Now())
	if got < 80*time.Second || got > 120*time.Second {
		t.Fatalf("open duration = %v, want within 100s ±20%%", got)
	}
}

func TestBreakerStore_OpenExpiryKeepsOpenCountUntilSuccess(t *testing.T) {
	s, clock := newTestBreakerStore(t, 1, 30*time.Second, 5*time.Minute, 0)
	// Pre-populate state as if a prior failure window expired one
	// second ago by anchoring it to the fake clock.
	s.states["deepseek-v4-pro"] = &breakerState{
		failures:  1,
		opens:     2,
		openUntil: clock.Now().Add(-time.Second),
	}

	if s.isOpen("deepseek-v4-pro") {
		t.Fatal("isOpen = true after expiry, want false")
	}
	b := s.states["deepseek-v4-pro"]
	if b.failures != 0 {
		t.Fatalf("failures after expiry = %d, want 0", b.failures)
	}
	if b.opens != 2 {
		t.Fatalf("opens after expiry = %d, want preserved count 2", b.opens)
	}

	s.recordSuccess("deepseek-v4-pro")
	if b.opens != 0 || b.failures != 0 || !b.openUntil.IsZero() {
		t.Fatalf("breaker after success = %+v, want full reset", *b)
	}
}

func TestBreakerStore_IsOpenFlipsAtExactCooldownBoundary(t *testing.T) {
	// Cooldown-window boundary is exercisable now that the clock is
	// controlled — pin isOpen behavior at exactly openUntil.
	s, clock := newTestBreakerStore(t, 1, 30*time.Second, time.Minute, 0)

	s.recordFailure("deepseek-v4-pro")
	if !s.isOpen("deepseek-v4-pro") {
		t.Fatal("isOpen = false immediately after open, want true")
	}
	clock.Advance(29 * time.Second)
	if !s.isOpen("deepseek-v4-pro") {
		t.Fatal("isOpen = false at 29s, want true (cooldown is 30s)")
	}
	clock.Advance(time.Second)
	if s.isOpen("deepseek-v4-pro") {
		t.Fatal("isOpen = true at exactly cooldown deadline, want false")
	}
}

func TestBreakerStore_DisabledByZeroConfig(t *testing.T) {
	s, _ := newTestBreakerStore(t, 0, 0, 0, 0)
	for i := 0; i < 5; i++ {
		s.recordFailure("any-model")
	}
	if s.isOpen("any-model") {
		t.Errorf("isOpen = true with disabled config, want false")
	}
}
