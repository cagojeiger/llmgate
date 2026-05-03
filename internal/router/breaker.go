package router

import (
	"log/slog"
	"math/rand"
	"sync"
	"time"
)

// breakerState is per-model failure tracking. failures resets on a
// success or on cooldown expiry; opens counts how many times the
// breaker has tripped without an intervening success, and is what
// drives the exponential cooldown growth. A single success resets
// opens to 0 (along with failures and openUntil) — chronic upstreams
// only climb the backoff ladder while they fail repeatedly.
type breakerState struct {
	failures  int
	openUntil time.Time
	opens     int
}

// breakerStore is a per-process, in-memory circuit breaker shared
// across goroutines via Router. The policy is "trip after N
// consecutive failures, cool down for an exponentially growing
// window, fully reset state on a clean success." There is
// intentionally no half-open / single-probe phase — once the
// cooldown expires, all callers proceed as if the breaker were
// closed. A real half-open belongs in a redis-backed implementation
// that can coordinate decisions across processes.
type breakerStore struct {
	mu     sync.Mutex
	states map[string]*breakerState
	cfg    breakerConfig
	log    *slog.Logger
}

// breakerConfig is the env-driven tuning the store applies. failureTrip
// or base <= 0 disables the breaker (every isOpen returns false). max
// caps the exponential backoff; jitter is symmetric (0.2 = ±20%).
type breakerConfig struct {
	failureTrip int
	base        time.Duration
	max         time.Duration
	jitter      float64
}

func newBreakerStore(cfg breakerConfig, log *slog.Logger) *breakerStore {
	return &breakerStore{
		states: map[string]*breakerState{},
		cfg:    cfg,
		log:    log,
	}
}

// isOpen reports whether modelID is currently in its cooldown window.
// On cooldown expiry the failure counter is reset (so the next attempt
// starts fresh) but `opens` is preserved so chronic upstreams continue
// to climb the exponential ladder.
func (s *breakerStore) isOpen(modelID string) bool {
	if s.cfg.base <= 0 || s.cfg.failureTrip <= 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.states[modelID]
	if !ok {
		return false
	}
	if !b.openUntil.IsZero() && time.Now().Before(b.openUntil) {
		return true
	}
	// On expiry, close the circuit but keep opens until a success so
	// repeated outages continue exponential backoff.
	if !b.openUntil.IsZero() {
		b.openUntil = time.Time{}
		b.failures = 0
	}
	return false
}

func (s *breakerStore) recordFailure(modelID string) {
	if s.cfg.failureTrip <= 0 || s.cfg.base <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.states[modelID]
	if !ok {
		b = &breakerState{}
		s.states[modelID] = b
	}
	b.failures++
	if b.failures >= s.cfg.failureTrip {
		b.opens++
		cooldown := s.nextOpenDurationLocked(b.opens)
		b.openUntil = time.Now().Add(cooldown)
		s.log.Warn("circuit opened",
			slog.String("model", modelID),
			slog.Int("opens", b.opens),
			slog.Duration("cooldown", cooldown),
		)
	}
}

func (s *breakerStore) recordSuccess(modelID string) {
	if s.cfg.failureTrip <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.states[modelID]; ok {
		b.failures = 0
		b.openUntil = time.Time{}
		b.opens = 0
	}
}

// nextOpenDurationLocked computes the current cooldown given the
// per-model `opens` count: the base duration doubled (opens-1) times,
// capped at max, then symmetrically jittered. Caller must hold s.mu.
func (s *breakerStore) nextOpenDurationLocked(opens int) time.Duration {
	base := s.cfg.base
	if base <= 0 {
		return 0
	}
	maxOpen := s.cfg.max
	if maxOpen <= 0 || maxOpen < base {
		maxOpen = base
	}

	cooldown := base
	for i := 1; i < opens; i++ {
		if cooldown >= maxOpen/2 {
			cooldown = maxOpen
			break
		}
		cooldown *= 2
		if cooldown > maxOpen {
			cooldown = maxOpen
			break
		}
	}

	jitter := s.cfg.jitter
	if jitter <= 0 {
		return cooldown
	}
	if jitter > 1 {
		jitter = 1
	}
	scale := 1 - jitter + rand.Float64()*(2*jitter)
	jittered := time.Duration(float64(cooldown) * scale)
	if jittered > maxOpen {
		return maxOpen
	}
	if jittered < 0 {
		return 0
	}
	return jittered
}
