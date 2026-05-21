package routing

import (
	"context"
	"log/slog"
	"time"

	"llmgate/internal/domain/llmtypes"
)

func (r *Service) CompleteStream(ctx context.Context, req *llmtypes.Request) (*RouteResult, error) {
	result := &RouteResult{}
	if req == nil {
		return result, &llmtypes.Error{Kind: llmtypes.KindBadRequest, Message: "request is nil"}
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
		attemptReq := *req
		attemptReq.Model = candidate.model
		att := llmtypes.Attempt{
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
func (r *Service) finalizeStreamFailure(result *RouteResult, candidate candidate, att *llmtypes.Attempt, err error, routeCtx context.Context) error {
	att.Kind = llmtypes.ErrorKindOf(err)
	att.StatusCode = llmtypes.StatusCodeOf(err)
	att.DurationMS = time.Since(att.StartedAt).Milliseconds()
	result.Attempts = append(result.Attempts, *att)
	result.Vendor = candidate.provider.Name()
	result.ModelUsed = candidate.model

	if !r.fallbackEligible(att.Kind) {
		return err
	}
	r.breakers.recordFailure(candidate.model)
	if rcErr := routeCtx.Err(); rcErr != nil {
		return contextError(rcErr)
	}
	r.log.Info("stream fallback triggered",
		slog.String("model", candidate.model),
		slog.String("error_kind", string(att.Kind)),
	)
	return nil
}
