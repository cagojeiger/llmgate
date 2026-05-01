package provider

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"llmgate/internal/catalog"
)

type AdapterFactory func(*catalog.Endpoint) (Provider, error)

// Router dispatches a Request to the right Provider based on model id,
// resolving aliases to ordered fallback chains and tracking per-process
// circuit-breaker state for each model.
//
// Fallback only applies to non-streaming Complete. CompleteStream uses
// the first chain entry; streaming fallback is intentionally out of V1
// scope (first-byte semantics + partial billing make it fragile —
// downgrade non-stream first, validate, then revisit).
type Router struct {
	byModel map[string]Provider
	aliases map[string][]string
	policy  fallbackPolicy
	log     *slog.Logger

	bMu      sync.Mutex
	breakers map[string]*breakerState
}

type fallbackPolicy struct {
	onKinds         map[Kind]struct{}
	circuitFailures int
	circuitOpen     time.Duration
}

type breakerState struct {
	failures  int
	openUntil time.Time
}

func NewRouter(cat *catalog.Catalog, factories map[string]AdapterFactory, log *slog.Logger) (*Router, error) {
	if log == nil {
		log = slog.Default()
	}

	byEndpoint := make(map[string]Provider, len(cat.Endpoints))
	for _, ep := range cat.Endpoints {
		factory, ok := factories[ep.Protocol]
		if !ok {
			log.Warn("no adapter for protocol", slog.String("protocol", ep.Protocol), slog.String("endpoint", ep.Name))
			continue
		}
		p, err := factory(ep)
		if err != nil {
			log.Warn("adapter factory failed",
				slog.String("protocol", ep.Protocol),
				slog.String("endpoint", ep.Name),
				slog.String("err", err.Error()),
			)
			continue
		}
		byEndpoint[ep.Name] = p
	}

	byModel := make(map[string]Provider, len(cat.Models))
	for modelID, model := range cat.Models {
		p, ok := byEndpoint[model.Endpoint]
		if !ok {
			continue
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

	policy := fallbackPolicy{
		onKinds:         make(map[Kind]struct{}, len(cat.Fallback.OnKinds)),
		circuitFailures: cat.Fallback.CircuitFailures,
		circuitOpen:     cat.Fallback.CircuitOpen,
	}
	for _, k := range cat.Fallback.OnKinds {
		policy.onKinds[Kind(strings.ToLower(k))] = struct{}{}
	}

	return &Router{
		byModel:  byModel,
		aliases:  aliases,
		policy:   policy,
		log:      log,
		breakers: make(map[string]*breakerState),
	}, nil
}

func (r *Router) Name() string { return "router" }

// Complete walks the fallback chain for the requested model. On a
// fallback-eligible error it tries the next entry; on a non-eligible
// error it returns immediately. Each upstream call is recorded as an
// Attempt on the request context (if a holder is installed) so audit
// can replay the chain. Skipped (circuit-open) models do not produce
// Attempt entries because no upstream call was made.
func (r *Router) Complete(ctx context.Context, req *Request) (*Response, error) {
	if req == nil {
		return nil, &Error{Kind: KindBadRequest, Message: "request is nil"}
	}

	chain, err := r.resolveChain(req.Model)
	if err != nil {
		return nil, err
	}

	var lastErr error
	calledAny := false

	for _, modelID := range chain {
		p, ok := r.byModel[modelID]
		if !ok {
			// Chain entry references a model the router couldn't
			// construct (e.g. its protocol factory failed). Skip
			// silently — operators see the warning at startup.
			continue
		}
		if r.isCircuitOpen(modelID) {
			r.log.Debug("skip model: circuit open", slog.String("model", modelID))
			continue
		}

		attemptReq := *req
		attemptReq.Model = modelID

		start := time.Now()
		resp, err := p.Complete(ctx, &attemptReq)
		dur := time.Since(start)
		calledAny = true

		att := Attempt{
			Vendor:     p.Name(),
			Model:      modelID,
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
			RecordAttempt(ctx, att)
			r.recordSuccess(modelID)
			return resp, nil
		}

		var perr *Error
		if errors.As(err, &perr) {
			att.ErrorKind = perr.Kind
			att.StatusCode = perr.StatusCode
		} else {
			att.ErrorKind = KindUnknown
		}
		RecordAttempt(ctx, att)
		lastErr = err

		if !r.fallbackEligible(att.ErrorKind) {
			return nil, err
		}
		r.recordFailure(modelID)
		r.log.Info("fallback triggered",
			slog.String("model", modelID),
			slog.String("error_kind", string(att.ErrorKind)),
		)
	}

	if !calledAny {
		// Every chain entry was either unregistered or had its circuit
		// open. Surface this as upstream-unavailable so callers can
		// distinguish from "request was bad".
		return nil, &Error{Kind: KindUpstream, Message: "all models in chain are currently unavailable"}
	}
	return nil, lastErr
}

// CompleteStream picks the first valid chain entry and dispatches once.
// V1 does not fall back streaming requests — see Router doc.
func (r *Router) CompleteStream(ctx context.Context, req *Request) (Stream, error) {
	if req == nil {
		return nil, &Error{Kind: KindBadRequest, Message: "request is nil"}
	}
	chain, err := r.resolveChain(req.Model)
	if err != nil {
		return nil, err
	}
	for _, modelID := range chain {
		p, ok := r.byModel[modelID]
		if !ok {
			continue
		}
		attemptReq := *req
		attemptReq.Model = modelID
		return p.CompleteStream(ctx, &attemptReq)
	}
	return nil, &Error{Kind: KindBadRequest, Message: "unknown model: " + req.Model}
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
	return nil, &Error{Kind: KindBadRequest, Message: "unknown model: " + model}
}

func (r *Router) fallbackEligible(k Kind) bool {
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
		b.openUntil = time.Now().Add(r.policy.circuitOpen)
		r.log.Warn("circuit opened",
			slog.String("model", modelID),
			slog.Duration("cooldown", r.policy.circuitOpen),
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
	}
}
