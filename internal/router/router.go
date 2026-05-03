// Package router resolves model aliases, applies fallback policy, and
// tracks per-process circuit-breaker state.
package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"llmgate/internal/catalog"
	"llmgate/internal/provider"
)

type AdapterFactory func(*catalog.Model) (provider.Provider, error)

// FallbackPolicy is env-driven router tuning. CircuitFailures or
// CircuitOpen <= 0 disables the breaker; CircuitJitter is symmetric
// (0.2 means ±20%).
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

type RouteResult struct {
	Response  *provider.Response
	Stream    provider.Stream
	Vendor    string
	ModelUsed string
	Attempts  []provider.Attempt
}

// Router dispatches requests to Providers. Streaming fallback applies
// only before the first event reaches the client.
type Router struct {
	byModel  map[string]provider.Provider
	aliases  map[string][]string
	policy   fallbackPolicy
	log      *slog.Logger
	breakers *breakerStore
}

type fallbackPolicy struct {
	onKinds            map[provider.Kind]struct{}
	requestTimeout     time.Duration
	completeTimeout    time.Duration
	streamStartTimeout time.Duration
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
		breakers: newBreakerStore(policy.CircuitFailures, policy.CircuitOpen, policy.CircuitMaxOpen, policy.CircuitJitter, log),
	}, nil
}

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
			r.breakers.recordSuccess(candidate.model)
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
		r.breakers.recordFailure(candidate.model)
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

func (r *Router) CompleteStream(ctx context.Context, req *provider.Request) (*RouteResult, error) {
	result := &RouteResult{}
	if req == nil {
		return result, &provider.Error{Kind: provider.KindBadRequest, Message: "request is nil"}
	}
	routeCtx := ctx
	cancelRoute := context.CancelFunc(func() {})
	if r.policy.requestTimeout > 0 {
		routeCtx, cancelRoute = context.WithTimeout(ctx, r.policy.requestTimeout)
	}
	// routeCtx is canceled by this defer unless ownership is transferred
	// to the returned stream on the success path below.
	routeOwned := true
	defer func() {
		if routeOwned {
			cancelRoute()
		}
	}()

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
		startCtx, cancelStart, stopStart, startTimedOut := streamStartContext(routeCtx, r.policy.streamStartTimeout)

		att := provider.Attempt{
			Vendor:    candidate.provider.Name(),
			Model:     candidate.model,
			StartedAt: time.Now(),
		}
		stream, err := candidate.provider.CompleteStream(startCtx, &attemptReq)
		if err != nil {
			stopStart()
			cancelStart()
			err = startTimeoutErr(err, startCtx, routeCtx, startTimedOut)
			lastErr = err
			if bail := r.finalizeStreamFailure(result, candidate, &att, err, routeCtx); bail != nil {
				return result, bail
			}
			continue
		}
		// Adapter's ValidateFirstEvent already proved the stream alive.
		// Stop the start-timer (without canceling startCtx) so the
		// established stream's underlying ctx survives.
		stopStart()

		result.Attempts = append(result.Attempts, att)
		streamCancel := cancelStart
		if r.policy.requestTimeout > 0 {
			streamCancel = func() { cancelStart(); cancelRoute() }
		}
		result.Stream = &cancelOnCloseStream{Stream: stream, cancel: streamCancel}
		result.Vendor = candidate.provider.Name()
		result.ModelUsed = candidate.model
		r.breakers.recordSuccess(candidate.model)
		routeOwned = false
		return result, nil
	}
	return result, lastErr
}

// finalizeStreamFailure stamps a failed stream attempt and decides
// whether to fall back. Returns the error to surface (caller returns)
// or nil (caller continues to next candidate).
func (r *Router) finalizeStreamFailure(result *RouteResult, candidate routeCandidate, att *provider.Attempt, err error, routeCtx context.Context) error {
	adoptAttemptError(att, err)
	att.DurationMS = time.Since(att.StartedAt).Milliseconds()
	result.Attempts = append(result.Attempts, *att)
	result.Vendor = candidate.provider.Name()
	result.ModelUsed = candidate.model

	if !r.fallbackEligible(att.ErrorKind) {
		return err
	}
	r.breakers.recordFailure(candidate.model)
	if rcErr := routeCtx.Err(); rcErr != nil {
		return contextError(rcErr)
	}
	r.log.Info("stream fallback triggered",
		slog.String("model", candidate.model),
		slog.String("error_kind", string(att.ErrorKind)),
	)
	return nil
}

// startTimeoutErr returns the disambiguated start-phase error, or the
// original adapter error when neither route ctx nor start timer fired.
func startTimeoutErr(orig error, startCtx, routeCtx context.Context, timedOut func() bool) error {
	if ctxErr := streamStartError(startCtx, routeCtx, timedOut); ctxErr != nil {
		return ctxErr
	}
	return orig
}

// streamStartContext bounds CompleteStream + first-event read with one
// timer. After both succeed the caller stops the timer (without
// cancelling ctx) so the returned stream's underlying ctx survives.
func streamStartContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc, func() bool, func() bool) {
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

// streamStartError disambiguates a pre-first-event failure: route-level
// cancellation > start-timeout > nil (caller falls back to original err).
func streamStartError(startCtx, routeCtx context.Context, timedOut func() bool) error {
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

// cancelOnCloseStream defers cancellation of the route-level ctx until
// the caller closes the stream.
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
		if r.breakers.isOpen(modelID) {
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
