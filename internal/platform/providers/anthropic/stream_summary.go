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
		ChunkCount:  s.ChunkCount,
		FirstByteAt: s.FirstByteAt,
	}
	if s.pendingFinish != nil {
		summary.FinishReason = s.pendingFinish.finishReason
		usage := &llmtypes.Usage{
			PromptTokens:     s.inputTokens,
			CompletionTokens: s.pendingFinish.outputTokens,
			TotalTokens:      s.inputTokens + s.pendingFinish.outputTokens,
		}
		addCacheUsageExtra(usage, s.pendingFinish.cacheCreationTokens, s.pendingFinish.cacheReadTokens)
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

func (s *stream) buildFinishEvent(end *anthropicEnd) *llmtypes.Event {
	if end == nil {
		end = &anthropicEnd{finishReason: "stop"}
	}
	usage := &llmtypes.Usage{
		PromptTokens:     s.inputTokens,
		CompletionTokens: end.outputTokens,
		TotalTokens:      s.inputTokens + end.outputTokens,
	}
	addCacheUsageExtra(usage, end.cacheCreationTokens, end.cacheReadTokens)
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
