package llmrouter

import (
	"log/slog"
	"math/rand"
	"sync"
	"time"
)

// breakerState tracks one model's consecutive failures. opens persists
// across cooldown expiry so chronic outages keep climbing the backoff
// ladder; only a clean success resets it.
type breakerState struct {
	failures  int
	openUntil time.Time
	opens     int
}

// breakerStore is per-process, in-memory. No half-open phase — once
// cooldown expires, all callers proceed as if closed. Cross-process
// coordination belongs in a redis-backed alternative.
type breakerStore struct {
	mu          sync.Mutex
	states      map[string]*breakerState
	failureTrip int
	base        time.Duration
	max         time.Duration
	jitter      float64
	log         *slog.Logger
}

func newBreakerStore(failureTrip int, base, max time.Duration, jitter float64, log *slog.Logger) *breakerStore {
	return &breakerStore{
		states:      map[string]*breakerState{},
		failureTrip: failureTrip,
		base:        base,
		max:         max,
		jitter:      jitter,
		log:         log,
	}
}

// disabled reports whether breaker logic is turned off by config. Used as
// a single source of truth so the three public methods stay in lockstep.
func (s *breakerStore) disabled() bool {
	return s.base <= 0 || s.failureTrip <= 0
}

func (s *breakerStore) isOpen(modelID string) bool {
	if s.disabled() {
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
	// Expired: reset failures so next attempt starts fresh; keep opens
	// so chronic outages keep climbing the backoff ladder.
	if !b.openUntil.IsZero() {
		b.openUntil = time.Time{}
		b.failures = 0
	}
	return false
}

func (s *breakerStore) recordFailure(modelID string) {
	if s.disabled() {
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
	if b.failures >= s.failureTrip {
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
	// base<=0 is also covered here, though states would already be empty
	// (only recordFailure populates s.states, and it bails on disabled()).
	if s.disabled() {
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

// nextOpenDurationLocked: base * 2^(opens-1), capped at max, then
// symmetrically jittered. Caller holds s.mu.
func (s *breakerStore) nextOpenDurationLocked(opens int) time.Duration {
	base := s.base
	if base <= 0 {
		return 0
	}
	maxOpen := s.max
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

	jitter := s.jitter
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
