package anthropic

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"llmgate/internal/llmtypes"
)

type anthropicSSEFixture struct {
	event   string
	payload string
}

func newAnthropicStreamServer(
	t *testing.T,
	check func(*testing.T, *http.Request),
	events ...anthropicSSEFixture,
) *localServer {
	t.Helper()
	return newLocalServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if check != nil {
			check(t, r)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range events {
			writeSSEEvent(t, w, event.event, event.payload)
		}
	}))
}

func openAnthropicTestStream(t *testing.T, server *localServer, model, prompt string) llmtypes.Stream {
	t.Helper()
	c := mustNew(t, Config{
		BaseURL:    server.URL,
		APIKey:     "test-key",
		HTTPClient: server.Client,
		Name:       "opencode",
	})
	stream, err := c.CompleteStream(context.Background(), &llmtypes.Request{
		Model:    model,
		Messages: []llmtypes.Message{{Role: "user", Content: prompt}},
	})
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	return stream
}

func messageStart(id, model string, inputTokens int) anthropicSSEFixture {
	return anthropicSSEFixture{
		event: "message_start",
		payload: fmt.Sprintf(`{
			"type": "message_start",
			"message": {
				"id": %q,
				"type": "message",
				"role": "assistant",
				"model": %q,
				"usage": {"input_tokens": %d}
			}
		}`, id, model, inputTokens),
	}
}

func pingEvent() anthropicSSEFixture {
	return anthropicSSEFixture{event: "ping", payload: `{"type":"ping"}`}
}

func textBlockStart(index int) anthropicSSEFixture {
	return anthropicSSEFixture{
		event: "content_block_start",
		payload: fmt.Sprintf(`{
			"type": "content_block_start",
			"index": %d,
			"content_block": {"type": "text", "text": ""}
		}`, index),
	}
}

func toolBlockStart(index int, id, name, input string) anthropicSSEFixture {
	return anthropicSSEFixture{
		event: "content_block_start",
		payload: fmt.Sprintf(`{
			"type": "content_block_start",
			"index": %d,
			"content_block": {
				"type": "tool_use",
				"id": %q,
				"name": %q,
				"input": %s
			}
		}`, index, id, name, input),
	}
}

func textDelta(index int, text string) anthropicSSEFixture {
	return contentBlockDelta(index, "text_delta", "text", text)
}

func thinkingDelta(index int, thinking string) anthropicSSEFixture {
	return contentBlockDelta(index, "thinking_delta", "thinking", thinking)
}

func inputJSONDelta(index int, partialJSON string) anthropicSSEFixture {
	return contentBlockDelta(index, "input_json_delta", "partial_json", partialJSON)
}

func contentBlockDelta(index int, deltaType, field, value string) anthropicSSEFixture {
	return anthropicSSEFixture{
		event: "content_block_delta",
		payload: fmt.Sprintf(`{
			"type": "content_block_delta",
			"index": %d,
			"delta": {
				"type": %q,
				"%s": %q
			}
		}`, index, deltaType, field, value),
	}
}

func blockStop(index int) anthropicSSEFixture {
	return anthropicSSEFixture{
		event:   "content_block_stop",
		payload: fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, index),
	}
}

func messageDelta(stopReason string, outputTokens int) anthropicSSEFixture {
	return messageDeltaWithCache(stopReason, outputTokens, 0, 0)
}

func messageDeltaWithCache(
	stopReason string,
	outputTokens int,
	cacheCreationTokens int,
	cacheReadTokens int,
) anthropicSSEFixture {
	cache := ""
	if cacheCreationTokens > 0 || cacheReadTokens > 0 {
		cache = fmt.Sprintf(`,
				"cache_creation_input_tokens": %d,
				"cache_read_input_tokens": %d`, cacheCreationTokens, cacheReadTokens)
	}
	return anthropicSSEFixture{
		event: "message_delta",
		payload: fmt.Sprintf(`{
			"type": "message_delta",
			"delta": {
				"stop_reason": %q,
				"stop_sequence": null
			},
			"usage": {
				"output_tokens": %d%s
			}
		}`, stopReason, outputTokens, cache),
	}
}

func messageStop() anthropicSSEFixture {
	return anthropicSSEFixture{event: "message_stop", payload: `{"type":"message_stop"}`}
}

func streamError(errorType, message string) anthropicSSEFixture {
	return anthropicSSEFixture{
		event:   "error",
		payload: fmt.Sprintf(`{"type":"error","error":{"type":%q,"message":%q}}`, errorType, message),
	}
}
