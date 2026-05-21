// Package routing contains the transport-independent routing service:
// model alias resolution, fallback policy, and per-process circuit-breaker
// state. The package depends only on stdlib + provider abstractions — no
// HTTP, no yaml, no catalog. Wiring (catalog yaml → Models / Aliases) is
// the caller's responsibility, so Service stays standalone — any that any
// frontend (HTTP, CLI, queue, gRPC) can drive.
package routing

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"llmgate/internal/domain/llmtypes"
)

// Models maps a model id to the provider that serves it. The id is the
// stable identifier the caller sends in a request (or that an alias
// resolves to). Lookup is case-insensitive — Service lowercases keys
// internally so callers don't have to normalize.
type Models = map[string]llmtypes.Provider

// Aliases maps an alias name to the ordered chain of model ids
// Service should try in turn. A single-entry chain disables fallback
// (it acts the same as a raw model call). Lookup is case-insensitive.
type Aliases = map[string][]string

// FallbackPolicy is env-driven Service tuning. CircuitFailures or
// CircuitOpen <= 0 disables the breaker; CircuitJitter is symmetric
// (0.2 means ±20%). The total request budget lives in the caller's
// ctx (handler middleware); the Service owns the per-attempt non-stream
// budget. Streaming uses the caller's ctx end-to-end — first-event
// validation lives in the adapter via streaming.ValidateStreamStart.
type FallbackPolicy struct {
	OnKinds         []string
	CircuitFailures int
	CircuitOpen     time.Duration
	CircuitMaxOpen  time.Duration
	CircuitJitter   float64
	CompleteTimeout time.Duration
}

type RouteResult struct {
	Response  *llmtypes.Response
	Stream    llmtypes.Stream
	Vendor    string
	ModelUsed string
	Attempts  []llmtypes.Attempt
}

// Service routes requests to Providers. Streaming fallback applies
// only before the first event reaches the client.
type Service struct {
	byModel  map[string]llmtypes.Provider
	aliases  map[string][]string
	policy   fallbackPolicy
	log      *slog.Logger
	breakers *breakerStore
}

type fallbackPolicy struct {
	onKinds         map[llmtypes.ErrorKind]struct{}
	completeTimeout time.Duration
}

type candidate struct {
	model    string
	provider llmtypes.Provider
}

// NewService builds a Service from already-instantiated providers.
// The caller is expected to have walked whatever data source it uses
// (yaml catalog, in-memory config, …) and produced the Models map +
// Aliases map. An empty Models map fails fast — there is nothing to
// route to.
func NewService(models Models, aliases Aliases, policy FallbackPolicy, log *slog.Logger) (*Service, error) {
	if log == nil {
		log = slog.Default()
	}
	if len(models) == 0 {
		return nil, errors.New("llmrouter: no models registered")
	}

	byModel := make(map[string]llmtypes.Provider, len(models))
	for id, p := range models {
		if p == nil {
			return nil, errors.New("llmrouter: model " + id + " has nil provider")
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
		onKinds:         make(map[llmtypes.ErrorKind]struct{}, len(policy.OnKinds)),
		completeTimeout: policy.CompleteTimeout,
	}
	for _, k := range policy.OnKinds {
		internalPolicy.onKinds[llmtypes.ErrorKind(strings.ToLower(k))] = struct{}{}
	}

	return &Service{
		byModel:  byModel,
		aliases:  aliasMap,
		policy:   internalPolicy,
		log:      log,
		breakers: newBreakerStore(policy.CircuitFailures, policy.CircuitOpen, policy.CircuitMaxOpen, policy.CircuitJitter, log),
	}, nil
}

func (r *Service) candidates(model string) ([]candidate, error) {
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
		return nil, &llmtypes.Error{Kind: llmtypes.KindUpstream, Message: "all models in chain are currently unavailable"}
	}
	return out, nil
}

// resolveChain expands aliases; raw model ids become a one-item chain.
func (r *Service) resolveChain(model string) ([]string, error) {
	key := strings.ToLower(model)
	if chain, ok := r.aliases[key]; ok {
		return chain, nil
	}
	if _, ok := r.byModel[key]; ok {
		return []string{key}, nil
	}
	return nil, &llmtypes.Error{Kind: llmtypes.KindBadRequest, Message: "unknown model: " + model}
}

func (r *Service) fallbackEligible(k llmtypes.ErrorKind) bool {
	if k == "" {
		return false
	}
	_, ok := r.policy.onKinds[k]
	return ok
}

// contextError reclassifies a ctx.Err() so callers surface KindTimeout
// instead of a bare context.DeadlineExceeded.
func contextError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &llmtypes.Error{Kind: llmtypes.KindTimeout, Message: err.Error(), Cause: err}
	}
	return err
}
