package routing

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"llmgate/internal/domain/llmtypes"
)

// TranscribeResult is the transcription counterpart of RouteResult. It carries
// the typed response plus the vendor/model/attempt history so the handler can
// audit a transcription request exactly like a chat completion.
type TranscribeResult struct {
	Response  *llmtypes.TranscriptionResponse
	Stream    llmtypes.TranscriptionStream
	Vendor    string
	ModelUsed string
	Attempts  []llmtypes.Attempt
}

type transcribeCandidate struct {
	model    string
	provider llmtypes.TranscriptionProvider
}

// Transcribe resolves the model/alias to a transcription chain and runs it
// through the same circuit-breaker + fallback policy as Complete. It is a
// parallel method rather than a shared generic skeleton: the per-attempt body
// (provider.Transcribe, TranscriptionResponse) differs from chat, while the
// chain-resolution and breaker machinery are reused from breaker.go and the
// shared fallbackEligible / contextError helpers.
func (r *Service) Transcribe(ctx context.Context, req *llmtypes.TranscriptionRequest) (*TranscribeResult, error) {
	result := &TranscribeResult{}
	if req == nil {
		return result, &llmtypes.Error{Kind: llmtypes.KindBadRequest, Message: "request is nil"}
	}

	candidates, err := r.transcribeCandidates(req.Model)
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
		attemptCtx, cancelAttempt := context.WithTimeout(ctx, r.policy.completeTimeout)

		start := time.Now()
		resp, err := candidate.provider.Transcribe(attemptCtx, &attemptReq)
		dur := time.Since(start)
		cancelAttempt()

		att := llmtypes.Attempt{
			Vendor:     candidate.provider.Name(),
			Model:      candidate.model,
			StartedAt:  start,
			DurationMS: dur.Milliseconds(),
		}
		if err == nil {
			att.StatusCode = 200
			result.Attempts = append(result.Attempts, att)
			result.Response = resp
			result.Vendor = candidate.provider.Name()
			result.ModelUsed = candidate.model
			r.breakers.recordSuccess(candidate.model)
			return result, nil
		}

		att.Kind = llmtypes.ErrorKindOf(err)
		att.StatusCode = llmtypes.StatusCodeOf(err)
		result.Attempts = append(result.Attempts, att)
		result.Vendor = candidate.provider.Name()
		result.ModelUsed = candidate.model
		lastErr = err

		if !r.fallbackEligible(att.Kind) {
			return result, err
		}
		r.breakers.recordFailure(candidate.model)
		if err := ctx.Err(); err != nil {
			return result, contextError(err)
		}
		r.log.Info("transcription fallback triggered",
			slog.String("model", candidate.model),
			slog.String("error_kind", string(att.Kind)),
		)
	}

	return result, lastErr
}

// TranscribeStream is the streaming counterpart of Transcribe. It reuses the
// same alias→chain + circuit-breaker + fallback skeleton, but a successful
// attempt hands back an open TranscriptionStream (finalized by the caller once
// drained) rather than a typed response. Streaming fallback applies only to
// pre-stream failures — once the stream opens the client owns the wire.
func (r *Service) TranscribeStream(ctx context.Context, req *llmtypes.TranscriptionRequest) (*TranscribeResult, error) {
	result := &TranscribeResult{}
	if req == nil {
		return result, &llmtypes.Error{Kind: llmtypes.KindBadRequest, Message: "request is nil"}
	}

	candidates, err := r.transcribeCandidates(req.Model)
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
		stream, err := candidate.provider.TranscribeStream(ctx, &attemptReq)
		if err != nil {
			lastErr = err
			if bail := r.finalizeTranscribeStreamFailure(result, candidate, &att, err, ctx); bail != nil {
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

// finalizeTranscribeStreamFailure stamps a failed stream-open attempt and
// decides whether to fall back. Mirrors finalizeStreamFailure for the
// transcription candidate type.
func (r *Service) finalizeTranscribeStreamFailure(result *TranscribeResult, candidate transcribeCandidate, att *llmtypes.Attempt, err error, routeCtx context.Context) error {
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
	r.log.Info("transcription stream fallback triggered",
		slog.String("model", candidate.model),
		slog.String("error_kind", string(att.Kind)),
	)
	return nil
}

// transcribeCandidates mirrors candidates() but resolves against the
// transcription provider map. The breaker check is identical — a model id's
// breaker state is shared regardless of which surface opened it.
func (r *Service) transcribeCandidates(model string) ([]transcribeCandidate, error) {
	chain, err := r.resolveTranscribeChain(model)
	if err != nil {
		return nil, err
	}
	out := make([]transcribeCandidate, 0, len(chain))
	for _, modelID := range chain {
		p, ok := r.byTranscribe[modelID]
		if !ok {
			r.log.Warn("skip model: transcription provider missing", slog.String("model", modelID))
			continue
		}
		if r.breakers.isOpen(modelID) {
			r.log.Debug("skip model: circuit open", slog.String("model", modelID))
			continue
		}
		out = append(out, transcribeCandidate{model: modelID, provider: p})
	}
	if len(out) == 0 {
		return nil, &llmtypes.Error{Kind: llmtypes.KindUpstream, Message: "all models in chain are currently unavailable"}
	}
	return out, nil
}

// resolveTranscribeChain expands aliases (shared with chat); a raw model id
// becomes a one-item chain only when it names a registered transcription
// provider, so a chat-only id cannot leak onto this surface.
func (r *Service) resolveTranscribeChain(model string) ([]string, error) {
	key := strings.ToLower(model)
	if chain, ok := r.aliases[key]; ok {
		return chain, nil
	}
	if _, ok := r.byTranscribe[key]; ok {
		return []string{key}, nil
	}
	return nil, &llmtypes.Error{Kind: llmtypes.KindBadRequest, Message: "unknown model: " + model}
}
