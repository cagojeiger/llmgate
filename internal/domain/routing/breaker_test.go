package routing

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

func newTestBreakerStore(failureTrip int, base, max time.Duration, jitter float64) *breakerStore {
	return newBreakerStore(failureTrip, base, max, jitter, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestBreakerStore_BackoffIncreasesAndCaps(t *testing.T) {
	s := newTestBreakerStore(1, 30*time.Second, 2*time.Minute, 0)

	before := time.Now()
	s.recordFailure("deepseek-v4-pro")
	first := s.states["deepseek-v4-pro"].openUntil.Sub(before)
	if first < 30*time.Second || first > 31*time.Second {
		t.Fatalf("first open duration = %v, want about 30s", first)
	}

	before = time.Now()
	s.recordFailure("deepseek-v4-pro")
	second := s.states["deepseek-v4-pro"].openUntil.Sub(before)
	if second < 60*time.Second || second > 61*time.Second {
		t.Fatalf("second open duration = %v, want about 60s", second)
	}

	before = time.Now()
	s.recordFailure("deepseek-v4-pro")
	third := s.states["deepseek-v4-pro"].openUntil.Sub(before)
	if third < 2*time.Minute || third > 2*time.Minute+time.Second {
		t.Fatalf("third open duration = %v, want capped at about 2m", third)
	}
}

func TestBreakerStore_JitterStaysInRange(t *testing.T) {
	s := newTestBreakerStore(1, 100*time.Second, 5*time.Minute, 0.2)

	before := time.Now()
	s.recordFailure("deepseek-v4-pro")
	got := s.states["deepseek-v4-pro"].openUntil.Sub(before)
	if got < 80*time.Second || got > 120*time.Second {
		t.Fatalf("open duration = %v, want within 100s +/-20%%", got)
	}
}

func TestBreakerStore_OpenExpiryKeepsOpenCountUntilSuccess(t *testing.T) {
	s := newTestBreakerStore(1, 30*time.Second, 5*time.Minute, 0)
	// Pre-populate as if a prior failure window expired in the past.
	s.states["deepseek-v4-pro"] = &breakerState{
		failures:  1,
		opens:     2,
		openUntil: time.Now().Add(-time.Second),
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

func TestBreakerStore_DisabledByZeroConfig(t *testing.T) {
	s := newTestBreakerStore(0, 0, 0, 0)
	for i := 0; i < 5; i++ {
		s.recordFailure("any-model")
	}
	if s.isOpen("any-model") {
		t.Errorf("isOpen = true with disabled config, want false")
	}
}
