// Package router resolves model aliases, applies fallback policy, and tracks
// per-process circuit-breaker state.
package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"llmgate/internal/catalog"
	"llmgate/internal/provider"
)

// AdapterFactory builds one Provider for one catalog Model.
type AdapterFactory func(*catalog.Model) (provider.Provider, error)

// FallbackPolicy is env-driven router tuning. OnKinds controls which
// provider errors can advance the chain. CircuitFailures or CircuitOpen <= 0
// disables the breaker. CircuitJitter is symmetric, so 0.2 means +/-20%.
type FallbackPolicy struct {
	OnKinds            []string
	CircuitFailures    int
	CircuitOpen        time.Duration
	CircuitMaxOpen     time.Duration
	CircuitJitter      float64
	RequestTimeout     time.Duration
	CompleteTimeout    time.Duration
	StreamStartTimeout time.Duration
}

// RouteResult carries the chosen response or stream plus the attempts made
// before success or final failure.
type RouteResult struct {
	Response   *provider.Response
	Stream     provider.Stream
	FirstEvent *provider.Event
	Vendor     string
	ModelUsed  string
	Attempts   []provider.Attempt
}

// Router dispatches requests to Providers and maintains breaker state.
// Streaming fallback only applies before the first event reaches the client.
type Router struct {
	byModel map[string]provider.Provider
	aliases map[string][]string
	policy  fallbackPolicy
	log     *slog.Logger

	bMu      sync.Mutex
	breakers map[string]*breakerState
}

type fallbackPolicy struct {
	onKinds            map[provider.Kind]struct{}
	circuitFailures    int
	circuitOpen        time.Duration
	circuitMaxOpen     time.Duration
	circuitJitter      float64
	requestTimeout     time.Duration
	completeTimeout    time.Duration
	streamStartTimeout time.Duration
}

type breakerState struct {
	failures  int
	openUntil time.Time
	opens     int
}

type routeCandidate struct {
	model    string
	provider provider.Provider
}

func NewRouter(cat *catalog.Catalog, factories map[string]AdapterFactory, policy FallbackPolicy, log *slog.Logger) (*Router, error) {
	if log == nil {
		log = slog.Default()
	}

	byModel := make(map[string]provider.Provider, len(cat.Models))
	for modelID, m := range cat.Models {
		factory, ok := factories[m.Protocol]
		if !ok {
			return nil, fmt.Errorf("router: no adapter for protocol %q (model %q)", m.Protocol, m.ID)
		}
		p, err := factory(m)
		if err != nil {
			return nil, fmt.Errorf("router: build adapter for model %q protocol %q: %w", m.ID, m.Protocol, err)
		}
		byModel[strings.ToLower(modelID)] = p
	}
	if len(byModel) == 0 {
		return nil, errors.New("router: no models registered (check protocol factories)")
	}

	aliases := make(map[string][]string, len(cat.Aliases))
	for name, a := range cat.Aliases {
		chain := make([]string, len(a.Chain))
		for i, m := range a.Chain {
			chain[i] = strings.ToLower(m)
		}
		aliases[strings.ToLower(name)] = chain
	}

	internalPolicy := fallbackPolicy{
		onKinds:            make(map[provider.Kind]struct{}, len(policy.OnKinds)),
		circuitFailures:    policy.CircuitFailures,
		circuitOpen:        policy.CircuitOpen,
		circuitMaxOpen:     policy.CircuitMaxOpen,
		circuitJitter:      policy.CircuitJitter,
		requestTimeout:     policy.RequestTimeout,
		completeTimeout:    policy.CompleteTimeout,
		streamStartTimeout: policy.StreamStartTimeout,
	}
	for _, k := range policy.OnKinds {
		internalPolicy.onKinds[provider.Kind(strings.ToLower(k))] = struct{}{}
	}

	return &Router{
		byModel:  byModel,
		aliases:  aliases,
		policy:   internalPolicy,
		log:      log,
		breakers: make(map[string]*breakerState),
	}, nil
}

// Complete walks the fallback chain for a non-stream request.
func (r *Router) Complete(ctx context.Context, req *provider.Request) (*RouteResult, error) {
	result := &RouteResult{}
	if req == nil {
		return result, &provider.Error{Kind: provider.KindBadRequest, Message: "request is nil"}
	}
	routeCtx := ctx
	if r.policy.requestTimeout > 0 {
		var cancel context.CancelFunc
		routeCtx, cancel = context.WithTimeout(ctx, r.policy.requestTimeout)
		defer cancel()
	}

	candidates, err := r.candidates(req.Model)
	if err != nil {
		return result, err
	}

	var lastErr error

	for _, candidate := range candidates {
		if err := routeCtx.Err(); err != nil {
			return result, contextError(err)
		}
		attemptReq := requestForCandidate(req, candidate)
		attemptCtx := routeCtx
		cancelAttempt := func() {}
		if r.policy.completeTimeout > 0 {
			attemptCtx, cancelAttempt = context.WithTimeout(routeCtx, r.policy.completeTimeout)
		}

		start := time.Now()
		resp, err := candidate.provider.Complete(attemptCtx, &attemptReq)
		dur := time.Since(start)
		cancelAttempt()

		att := provider.Attempt{
			Vendor:     candidate.provider.Name(),
			Model:      candidate.model,
			StartedAt:  start,
			DurationMS: dur.Milliseconds(),
		}
		if err == nil {
			att.StatusCode = 200
			if resp != nil {
				if resp.Usage != nil {
					att.Usage = resp.Usage
				}
				if cost, ok := resp.Extra["cost"]; ok && len(cost) > 0 {
					att.VendorCost = string(cost)
				}
			}
			result.Attempts = append(result.Attempts, att)
			result.Response = resp
			result.Vendor = candidate.provider.Name()
			result.ModelUsed = candidate.model
			r.recordSuccess(candidate.model)
			return result, nil
		}

		adoptAttemptError(&att, err)
		result.Attempts = append(result.Attempts, att)
		result.Vendor = candidate.provider.Name()
		result.ModelUsed = candidate.model
		lastErr = err

		if !r.fallbackEligible(att.ErrorKind) {
			return result, err
		}
		r.recordFailure(candidate.model)
		if err := routeCtx.Err(); err != nil {
			return result, contextError(err)
		}
		r.log.Info("fallback triggered",
			slog.String("model", candidate.model),
			slog.String("error_kind", string(att.ErrorKind)),
		)
	}

	return result, lastErr
}

// CompleteStream walks the fallback chain until the first stream event is
// received. After that, mid-stream errors are returned to the caller.
func (r *Router) CompleteStream(ctx context.Context, req *provider.Request) (*RouteResult, error) {
	result := &RouteResult{}
	if req == nil {
		return result, &provider.Error{Kind: provider.KindBadRequest, Message: "request is nil"}
	}
	routeCtx := ctx
	cancelRoute := func() {}
	if r.policy.requestTimeout > 0 {
		routeCtx, cancelRoute = context.WithTimeout(ctx, r.policy.requestTimeout)
	}

	candidates, err := r.candidates(req.Model)
	if err != nil {
		cancelRoute()
		return result, err
	}

	var lastErr error
	for _, candidate := range candidates {
		if err := routeCtx.Err(); err != nil {
			cancelRoute()
			return result, contextError(err)
		}
		attemptReq := requestForCandidate(req, candidate)
		startCtx, cancelStart, stopStart, streamStartTimedOut := streamStartContext(routeCtx, r.policy.streamStartTimeout)

		att := provider.Attempt{
			Vendor:    candidate.provider.Name(),
			Model:     candidate.model,
			StartedAt: time.Now(),
		}
		stream, err := candidate.provider.CompleteStream(startCtx, &attemptReq)
		if err != nil {
			stopStart()
			cancelStart()
			adoptAttemptError(&att, err)
			if ctxErr := streamStartError(startCtx, routeCtx, streamStartTimedOut); ctxErr != nil {
				adoptAttemptError(&att, ctxErr)
				err = ctxErr
			}
			att.DurationMS = time.Since(att.StartedAt).Milliseconds()
			result.Attempts = append(result.Attempts, att)
			result.Vendor = candidate.provider.Name()
			result.ModelUsed = candidate.model
			lastErr = err

			if !r.fallbackEligible(att.ErrorKind) {
				cancelRoute()
				return result, err
			}
			r.recordFailure(candidate.model)
			if err := routeCtx.Err(); err != nil {
				cancelRoute()
				return result, contextError(err)
			}
			r.log.Info("stream fallback triggered",
				slog.String("model", candidate.model),
				slog.String("error_kind", string(att.ErrorKind)),
			)
			continue
		}

		firstEvent, err := recvFirstEvent(startCtx, stream)
		if err != nil {
			_ = stream.Close()
			stopStart()
			cancelStart()
			adoptAttemptError(&att, err)
			if ctxErr := streamStartError(startCtx, routeCtx, streamStartTimedOut); ctxErr != nil {
				adoptAttemptError(&att, ctxErr)
				err = ctxErr
			}
			att.DurationMS = time.Since(att.StartedAt).Milliseconds()
			result.Attempts = append(result.Attempts, att)
			result.Vendor = candidate.provider.Name()
			result.ModelUsed = candidate.model
			lastErr = err

			if !r.fallbackEligible(att.ErrorKind) {
				cancelRoute()
				return result, err
			}
			r.recordFailure(candidate.model)
			if err := routeCtx.Err(); err != nil {
				cancelRoute()
				return result, contextError(err)
			}
			r.log.Info("stream fallback triggered",
				slog.String("model", candidate.model),
				slog.String("error_kind", string(att.ErrorKind)),
			)
			continue
		}
		if !stopStart() && streamStartTimedOut() {
			_ = stream.Close()
			cancelStart()
			err := streamStartError(startCtx, routeCtx, streamStartTimedOut)
			if err == nil {
				err = contextError(startCtx.Err())
			}
			adoptAttemptError(&att, err)
			att.DurationMS = time.Since(att.StartedAt).Milliseconds()
			result.Attempts = append(result.Attempts, att)
			result.Vendor = candidate.provider.Name()
			result.ModelUsed = candidate.model
			lastErr = err
			if !r.fallbackEligible(att.ErrorKind) {
				cancelRoute()
				return result, err
			}
			r.recordFailure(candidate.model)
			continue
		}

		result.Attempts = append(result.Attempts, att)
		streamCancel := cancelStart
		if r.policy.requestTimeout > 0 {
			streamCancel = func() {
				cancelStart()
				cancelRoute()
			}
		}
		result.Stream = &cancelOnCloseStream{Stream: stream, cancel: streamCancel}
		result.FirstEvent = firstEvent
		result.Vendor = candidate.provider.Name()
		result.ModelUsed = candidate.model
		r.recordSuccess(candidate.model)
		return result, nil
	}
	cancelRoute()
	return result, lastErr
}

type streamStartStopper func() bool
type streamStartTimedOut func() bool

func streamStartContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc, streamStartStopper, streamStartTimedOut) {
	ctx, cancel := context.WithCancel(parent)
	if timeout <= 0 {
		return ctx, cancel, func() bool { return true }, func() bool { return false }
	}
	var timedOut atomic.Bool
	timer := time.AfterFunc(timeout, func() {
		timedOut.Store(true)
		cancel()
	})
	return ctx, cancel, timer.Stop, timedOut.Load
}

func streamStartError(startCtx, routeCtx context.Context, timedOut streamStartTimedOut) error {
	if routeErr := routeCtx.Err(); routeErr != nil {
		return contextError(routeErr)
	}
	if timedOut != nil && timedOut() {
		return &provider.Error{Kind: provider.KindTimeout, Message: "stream start timeout"}
	}
	if err := startCtx.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return contextError(err)
	}
	return nil
}

type streamRecvResult struct {
	event *provider.Event
	err   error
}

// streamCloseGrace bounds how long the router waits for a goroutine
// running Stream.Recv to exit after Close has been called. The Stream
// contract requires Close to unblock Recv promptly; this grace is a
// defensive safety net so a misbehaving adapter cannot stall the
// caller indefinitely. The buffered channel keeps the goroutine itself
// from leaking the channel — the goroutine is abandoned but can still
// complete in the background.
var streamCloseGrace = 5 * time.Second

func recvFirstEvent(ctx context.Context, stream provider.Stream) (*provider.Event, error) {
	ch := make(chan streamRecvResult, 1)
	go func() {
		event, err := stream.Recv()
		ch <- streamRecvResult{event: event, err: err}
	}()

	select {
	case got := <-ch:
		return got.event, got.err
	case <-ctx.Done():
		_ = stream.Close()
		select {
		case <-ch:
		case <-time.After(streamCloseGrace):
		}
		return nil, contextError(ctx.Err())
	}
}

type cancelOnCloseStream struct {
	provider.Stream
	cancel context.CancelFunc
	once   sync.Once
}

func (s *cancelOnCloseStream) Close() error {
	err := s.Stream.Close()
	s.once.Do(s.cancel)
	return err
}

func requestForCandidate(req *provider.Request, candidate routeCandidate) provider.Request {
	attemptReq := *req
	attemptReq.Model = candidate.model
	return attemptReq
}

func adoptAttemptError(att *provider.Attempt, err error) {
	var perr *provider.Error
	if errors.As(err, &perr) {
		att.ErrorKind = perr.Kind
		att.StatusCode = perr.StatusCode
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		att.ErrorKind = provider.KindTimeout
		return
	}
	att.ErrorKind = provider.KindUnknown
}

func contextError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &provider.Error{Kind: provider.KindTimeout, Message: err.Error(), Cause: err}
	}
	return err
}

func (r *Router) candidates(model string) ([]routeCandidate, error) {
	chain, err := r.resolveChain(model)
	if err != nil {
		return nil, err
	}
	out := make([]routeCandidate, 0, len(chain))
	for _, modelID := range chain {
		p, ok := r.byModel[modelID]
		if !ok {
			continue
		}
		if r.isCircuitOpen(modelID) {
			r.log.Debug("skip model: circuit open", slog.String("model", modelID))
			continue
		}
		out = append(out, routeCandidate{
			model:    modelID,
			provider: p,
		})
	}
	if len(out) == 0 {
		return nil, &provider.Error{Kind: provider.KindUpstream, Message: "all models in chain are currently unavailable"}
	}
	return out, nil
}

// resolveChain expands aliases; raw model ids become a one-item chain.
func (r *Router) resolveChain(model string) ([]string, error) {
	key := strings.ToLower(model)
	if chain, ok := r.aliases[key]; ok {
		return chain, nil
	}
	if _, ok := r.byModel[key]; ok {
		return []string{key}, nil
	}
	return nil, &provider.Error{Kind: provider.KindBadRequest, Message: "unknown model: " + model}
}

func (r *Router) fallbackEligible(k provider.Kind) bool {
	if k == "" {
		return false
	}
	_, ok := r.policy.onKinds[k]
	return ok
}

func (r *Router) isCircuitOpen(modelID string) bool {
	if r.policy.circuitOpen <= 0 || r.policy.circuitFailures <= 0 {
		return false
	}
	r.bMu.Lock()
	defer r.bMu.Unlock()
	b, ok := r.breakers[modelID]
	if !ok {
		return false
	}
	if !b.openUntil.IsZero() && time.Now().Before(b.openUntil) {
		return true
	}
	// On expiry, close the circuit but keep opens until a success so repeated
	// outages continue exponential backoff.
	if !b.openUntil.IsZero() {
		b.openUntil = time.Time{}
		b.failures = 0
	}
	return false
}

func (r *Router) recordFailure(modelID string) {
	if r.policy.circuitFailures <= 0 || r.policy.circuitOpen <= 0 {
		return
	}
	r.bMu.Lock()
	defer r.bMu.Unlock()
	b, ok := r.breakers[modelID]
	if !ok {
		b = &breakerState{}
		r.breakers[modelID] = b
	}
	b.failures++
	if b.failures >= r.policy.circuitFailures {
		b.opens++
		cooldown := r.nextOpenDurationLocked(b.opens)
		b.openUntil = time.Now().Add(cooldown)
		r.log.Warn("circuit opened",
			slog.String("model", modelID),
			slog.Int("opens", b.opens),
			slog.Duration("cooldown", cooldown),
		)
	}
}

func (r *Router) recordSuccess(modelID string) {
	if r.policy.circuitFailures <= 0 {
		return
	}
	r.bMu.Lock()
	defer r.bMu.Unlock()
	if b, ok := r.breakers[modelID]; ok {
		b.failures = 0
		b.openUntil = time.Time{}
		b.opens = 0
	}
}

func (r *Router) nextOpenDurationLocked(opens int) time.Duration {
	base := r.policy.circuitOpen
	if base <= 0 {
		return 0
	}
	maxOpen := r.policy.circuitMaxOpen
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

	jitter := r.policy.circuitJitter
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
