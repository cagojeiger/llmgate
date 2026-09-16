package routing

import (
	"context"
	"strings"
	"time"

	"llmgate/internal/domain/llmtypes"
)

type EmbeddingModels = map[string]llmtypes.EmbeddingProvider

func WithEmbeddings(models EmbeddingModels) Option {
	return func(s *Service) {
		s.byEmbedding = make(EmbeddingModels, len(models))
		for id, provider := range models {
			if provider != nil {
				s.byEmbedding[strings.ToLower(id)] = provider
			}
		}
	}
}

type EmbedResult struct {
	Response  *llmtypes.EmbeddingResponse
	Vendor    string
	ModelUsed string
	Attempts  []llmtypes.Attempt
}

// Embed performs one model call. Cross-model fallback could silently change
// the vector space of an existing index, so an embedding alias has one target.
func (r *Service) Embed(ctx context.Context, req *llmtypes.EmbeddingRequest) (*EmbedResult, error) {
	result := &EmbedResult{}
	if err := req.Validate(); err != nil {
		return result, err
	}
	model := strings.ToLower(req.Model)
	if chain, ok := r.aliases[model]; ok {
		if len(chain) != 1 {
			return result, &llmtypes.Error{Kind: llmtypes.KindBadRequest, Message: "embedding aliases must have exactly one model"}
		}
		model = chain[0]
	}
	provider, ok := r.byEmbedding[model]
	if !ok {
		return result, &llmtypes.Error{Kind: llmtypes.KindBadRequest, Message: "unknown embedding model: " + req.Model}
	}
	if err := ctx.Err(); err != nil {
		return result, contextError(err)
	}
	if r.breakers.isOpen(model) {
		return result, &llmtypes.Error{Kind: llmtypes.KindUpstream, Message: "embedding model is temporarily unavailable"}
	}
	attemptCtx, cancel := context.WithTimeout(ctx, r.policy.completeTimeout)
	defer cancel()
	attemptReq := *req
	attemptReq.Model = model
	start := time.Now()
	resp, err := provider.Embed(attemptCtx, &attemptReq)
	if err == nil && resp == nil {
		err = &llmtypes.Error{Kind: llmtypes.KindEmpty, Message: "empty embedding response"}
	}
	att := llmtypes.Attempt{Vendor: provider.Name(), Model: model, StartedAt: start, DurationMS: time.Since(start).Milliseconds()}
	result.Vendor, result.ModelUsed = provider.Name(), model
	if err != nil {
		att.Kind, att.StatusCode = llmtypes.ErrorKindOf(err), llmtypes.StatusCodeOf(err)
		if r.fallbackEligible(att.Kind) {
			r.breakers.recordFailure(model)
		}
	} else {
		att.StatusCode, att.Usage = 200, resp.Usage
		result.Response = resp
		r.breakers.recordSuccess(model)
	}
	result.Attempts = []llmtypes.Attempt{att}
	return result, err
}
