// Package dispatch resolves model aliases, applies fallback policy, and
// tracks per-process circuit-breaker state. The package depends only on
// stdlib + provider abstractions — no HTTP, no yaml, no catalog. Wiring
// (catalog yaml → Models / Aliases) is the caller's responsibility, so
// Dispatcher stays a standalone service that any frontend (HTTP, CLI,
// queue, gRPC) can drive.
package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"llmgate/internal/provider"
)

// Models maps a model id to the provider that serves it. The id is the
// stable identifier the caller sends in a request (or that an alias
// resolves to). Lookup is case-insensitive — Dispatcher lowercases keys
// internally so callers don't have to normalize.
type Models = map[string]provider.Provider

// Aliases maps an alias name to the ordered chain of model ids
// Dispatcher should try in turn. A single-entry chain disables fallback
// (it acts the same as a raw model call). Lookup is case-insensitive.
type Aliases = map[string][]string

// FallbackPolicy is env-driven dispatcher tuning. CircuitFailures or
// CircuitOpen <= 0 disables the breaker; CircuitJitter is symmetric
// (0.2 means ±20%). The total request budget lives in the caller's
// ctx (handler middleware); dispatcher owns the per-attempt non-stream
// budget. Streaming uses the caller's ctx end-to-end — first-event
// validation lives in the adapter via provider.ValidateFirstEvent.
type FallbackPolicy struct {
	OnKinds         []string
	CircuitFailures int
	CircuitOpen     time.Duration
	CircuitMaxOpen  time.Duration
	CircuitJitter   float64
	CompleteTimeout time.Duration
}

type Result struct {
	Response  *provider.Response
	Stream    provider.Stream
	Vendor    string
	ModelUsed string
	Attempts  []provider.Attempt
}

// Dispatcher dispatches requests to Providers. Streaming fallback applies
// only before the first event reaches the client.
type Dispatcher struct {
	byModel  map[string]provider.Provider
	aliases  map[string][]string
	policy   fallbackPolicy
	log      *slog.Logger
	breakers *breakerStore
}

type fallbackPolicy struct {
	onKinds         map[provider.Kind]struct{}
	completeTimeout time.Duration
}

type candidate struct {
	model    string
	provider provider.Provider
}

// NewDispatcher builds a dispatcher from already-instantiated providers.
// The caller is expected to have walked whatever data source it uses
// (yaml catalog, in-memory config, …) and produced the Models map +
// Aliases map. An empty Models map fails fast — there is nothing to
// dispatch to.
func NewDispatcher(models Models, aliases Aliases, policy FallbackPolicy, log *slog.Logger) (*Dispatcher, error) {
	if log == nil {
		log = slog.Default()
	}
	if len(models) == 0 {
		return nil, errors.New("dispatch: no models registered")
	}

	byModel := make(map[string]provider.Provider, len(models))
	for id, p := range models {
		if p == nil {
			return nil, errors.New("dispatch: model " + id + " has nil provider")
		}
		byModel[strings.ToLower(id)] = p
	}

	aliasMap := make(map[string][]string, len(aliases))
	for name, chain := range aliases {
		normalized := make([]string, len(chain))
		for i, m := range chain {
			normalized[i] = strings.ToLower(m)
		}
		aliasMap[strings.ToLower(name)] = normalized
	}

	internalPolicy := fallbackPolicy{
		onKinds:         make(map[provider.Kind]struct{}, len(policy.OnKinds)),
		completeTimeout: policy.CompleteTimeout,
	}
	for _, k := range policy.OnKinds {
		internalPolicy.onKinds[provider.Kind(strings.ToLower(k))] = struct{}{}
	}

	return &Dispatcher{
		byModel:  byModel,
		aliases:  aliasMap,
		policy:   internalPolicy,
		log:      log,
		breakers: newBreakerStore(policy.CircuitFailures, policy.CircuitOpen, policy.CircuitMaxOpen, policy.CircuitJitter, log),
	}, nil
}

func (r *Dispatcher) Complete(ctx context.Context, req *provider.Request) (*Result, error) {
	result := &Result{}
	if req == nil {
		return result, &provider.Error{Kind: provider.KindBadRequest, Message: "request is nil"}
	}

	candidates, err := r.candidates(req.Model)
	if err != nil {
		return result, err
	}

	var lastErr error

	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return result, contextError(err)
		}
		attemptReq := requestForCandidate(req, candidate)
		attemptCtx := ctx
		cancelAttempt := func() {}
		if r.policy.completeTimeout > 0 {
			attemptCtx, cancelAttempt = context.WithTimeout(ctx, r.policy.completeTimeout)
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
		if err := ctx.Err(); err != nil {
			return result, contextError(err)
		}
		r.log.Info("fallback triggered",
			slog.String("model", candidate.model),
			slog.String("error_kind", string(att.ErrorKind)),
		)
	}

	return result, lastErr
}

func (r *Dispatcher) CompleteStream(ctx context.Context, req *provider.Request) (*Result, error) {
	result := &Result{}
	if req == nil {
		return result, &provider.Error{Kind: provider.KindBadRequest, Message: "request is nil"}
	}

	candidates, err := r.candidates(req.Model)
	if err != nil {
		return result, err
	}

	var lastErr error
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return result, contextError(err)
		}
		attemptReq := requestForCandidate(req, candidate)
		att := provider.Attempt{
			Vendor:    candidate.provider.Name(),
			Model:     candidate.model,
			StartedAt: time.Now(),
		}
		stream, err := candidate.provider.CompleteStream(ctx, &attemptReq)
		if err != nil {
			lastErr = err
			if bail := r.finalizeStreamFailure(result, candidate, &att, err, ctx); bail != nil {
				return result, bail
			}
			continue
		}

		result.Attempts = append(result.Attempts, att)
		result.Stream = stream
		result.Vendor = candidate.provider.Name()
		result.ModelUsed = candidate.model
		r.breakers.recordSuccess(candidate.model)
		return result, nil
	}
	return result, lastErr
}

// finalizeStreamFailure stamps a failed stream attempt and decides
// whether to fall back. Returns the error to surface (caller returns)
// or nil (caller continues to next candidate).
func (r *Dispatcher) finalizeStreamFailure(result *Result, candidate candidate, att *provider.Attempt, err error, routeCtx context.Context) error {
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

func requestForCandidate(req *provider.Request, candidate candidate) provider.Request {
	attemptReq := *req
	attemptReq.Model = candidate.model
	return attemptReq
}

func adoptAttemptError(att *provider.Attempt, err error) {
	att.ErrorKind = provider.KindOf(err)
	att.StatusCode = provider.StatusCodeOf(err)
}

func contextError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &provider.Error{Kind: provider.KindTimeout, Message: err.Error(), Cause: err}
	}
	return err
}

func (r *Dispatcher) candidates(model string) ([]candidate, error) {
	chain, err := r.resolveChain(model)
	if err != nil {
		return nil, err
	}
	out := make([]candidate, 0, len(chain))
	for _, modelID := range chain {
		p, ok := r.byModel[modelID]
		if !ok {
			continue
		}
		if r.breakers.isOpen(modelID) {
			r.log.Debug("skip model: circuit open", slog.String("model", modelID))
			continue
		}
		out = append(out, candidate{
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
func (r *Dispatcher) resolveChain(model string) ([]string, error) {
	key := strings.ToLower(model)
	if chain, ok := r.aliases[key]; ok {
		return chain, nil
	}
	if _, ok := r.byModel[key]; ok {
		return []string{key}, nil
	}
	return nil, &provider.Error{Kind: provider.KindBadRequest, Message: "unknown model: " + model}
}

func (r *Dispatcher) fallbackEligible(k provider.Kind) bool {
	if k == "" {
		return false
	}
	_, ok := r.policy.onKinds[k]
	return ok
}
