// Package router resolves a logical model name to an ordered chain of
// concrete adapter calls, applies fallback policy on eligible upstream
// errors, and tracks per-process circuit-breaker state. The package is
// the only consumer of catalog policy at runtime; the provider package
// stays a pure adapter contract.
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

// AdapterFactory builds one Provider for one Model. The factory resolves
// the credential env (m.AuthEnv) at call time and passes the value into the
// adapter — keeping env reads out of the catalog package.
type AdapterFactory func(*catalog.Model) (provider.Provider, error)

// FallbackPolicy is the runtime tuning the router applies to every
// alias chain. It does not live in catalog yaml because it has nothing
// to do with vendor or model data — it shapes how the algorithm reacts
// to upstream errors. main.go assembles it from env-driven config and
// passes it in.
//
// OnKinds is matched against provider.Kind. When the list is empty
// fallback is effectively disabled (no error class is eligible to
// advance the chain). CircuitFailures<=0 or CircuitOpen<=0 disables
// the per-process circuit breaker; otherwise N consecutive failures
// trip a model and skip it for that duration. CircuitMaxOpen caps
// repeated-open exponential backoff. CircuitJitter applies symmetric
// randomization, where 0.2 means +/-20%.
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

// RouteResult is the outcome of one Router.Complete or Router.CompleteStream
// call. Exactly one of Response/Stream is populated on success. On failure
// the response side is nil but Attempts is still populated so audit can
// log the partial chain.
//
// For Complete: Attempts records every upstream call in chain order; the
// last entry corresponds to the body returned (success) or the final
// failure. Vendor/ModelUsed reflect that last entry.
//
// For CompleteStream: the final attempt's stream is started but not yet
// drained, so that Attempt has StartedAt set and FirstEvent contains the
// pre-read event used to prove the stream really started before bytes are
// sent to the client. The caller must send FirstEvent, then drain Stream
// and finalize the Attempt at end-of-stream from Stream.Summary() and any
// Recv error.
type RouteResult struct {
	Response   *provider.Response
	Stream     provider.Stream
	FirstEvent *provider.Event
	Vendor     string
	ModelUsed  string
	Attempts   []provider.Attempt
}

// Router dispatches a Request to the right Provider based on model id,
// resolving aliases to ordered fallback chains and tracking per-process
// circuit-breaker state for each model.
//
// Fallback applies to non-streaming Complete and to CompleteStream only
// before a stream is established. Once streaming starts, mid-stream
// fallback is intentionally out of scope: partial output may already
// have reached the caller.
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

// Complete walks the fallback chain for the requested model. On a
// fallback-eligible error it tries the next entry; on a non-eligible
// error it returns immediately. Each upstream call is appended to
// RouteResult.Attempts so audit can replay the chain. Skipped
// (circuit-open) models do not produce Attempt entries because no
// upstream call was made.
//
// RouteResult is non-nil for every return; Attempts is populated even
// on error so the caller can audit the partial chain. The error is
// non-nil iff no chain entry produced a body.
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

// CompleteStream walks the chain until a stream is established. It can
// fall back on pre-stream failures because no SSE bytes have reached the
// caller yet. After returning Stream, mid-stream Recv failures are not
// routed through fallback.
//
// On success the returned RouteResult has Stream populated and the final
// Attempt has StartedAt set; the caller drains the stream and finalizes
// that Attempt (DurationMS, Usage, VendorCost, ErrorKind) at end-of-stream.
// On pre-stream errors, Attempts are finalized before fallback or return.
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
		<-ch
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

// resolveChain returns the lowercased chain for a model name. Aliases
// expand to their declared chain; raw model ids resolve to a one-element
// chain. Returns BadRequest when the name is neither a known alias nor a
// known model.
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
	// expired open window — half-open: allow one attempt by resetting state.
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
