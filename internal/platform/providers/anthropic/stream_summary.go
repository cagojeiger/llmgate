package anthropic

import "llmgate/internal/domain/llmtypes"

type anthropicEnd struct {
	finishReason        string
	outputTokens        int
	cacheCreationTokens int
	cacheReadTokens     int
}

func (s *stream) Summary() *llmtypes.Summary {
	summary := &llmtypes.Summary{
		Model:       s.msgModel,
		VendorCost:  string(s.vendorCost),
		ChunkCount:  s.ChunkCount,
		FirstByteAt: s.FirstByteAt,
	}
	if s.pendingFinish != nil {
		summary.FinishReason = s.pendingFinish.finishReason
		usage := s.buildUsage(s.pendingFinish)
		llmtypes.AttachUsageCost(usage, s.vendorCost, s.cost)
		summary.Usage = usage
	} else if s.inputTokens > 0 {
		// Partial streams still expose prompt token consumption to audit.
		summary.Usage = &llmtypes.Usage{
			PromptTokens: s.inputTokens,
			TotalTokens:  s.inputTokens,
		}
	}
	return summary
}

// buildFinishEvent assembles the final chunk from a non-nil end. The two
// production callers gate on s.pendingFinish != nil immediately above,
// so a nil end is unreachable and intentionally not defended here.
func (s *stream) buildFinishEvent(end *anthropicEnd) *llmtypes.Event {
	usage := s.buildUsage(end)
	s.costSource = llmtypes.AttachUsageCost(usage, s.vendorCost, s.cost)
	return &llmtypes.Event{
		ID:     s.msgID,
		Object: "chat.completion.chunk",
		Model:  s.msgModel,
		Choices: []llmtypes.ChoiceDelta{{
			Index:        0,
			Delta:        llmtypes.Delta{},
			FinishReason: end.finishReason,
		}},
		Usage: usage,
	}
}

func (s *stream) buildUsage(end *anthropicEnd) *llmtypes.Usage {
	usage := &llmtypes.Usage{
		PromptTokens:     s.inputTokens,
		CompletionTokens: end.outputTokens,
		TotalTokens:      s.inputTokens + end.outputTokens,
	}
	addCacheUsageExtra(usage, end.cacheCreationTokens, end.cacheReadTokens)
	return usage
}
