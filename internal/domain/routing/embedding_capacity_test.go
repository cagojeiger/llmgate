package routing

import (
	"context"
	"llmgate/internal/domain/llmtypes"
	"testing"
)

type capacityEmbedding struct {
	calls    int
	capacity bool
}

func (p *capacityEmbedding) Name() string { return "worker" }
func (p *capacityEmbedding) Embed(context.Context, *llmtypes.EmbeddingRequest) (*llmtypes.EmbeddingResponse, error) {
	p.calls++
	return nil, &llmtypes.Error{Kind: llmtypes.KindRateLimit, StatusCode: 429, WorkerCapacity: p.capacity}
}
func TestEmbeddingWorkerCapacityPreservesOrdinaryRateLimitPolicy(t *testing.T) {
	for _, capacity := range []bool{true, false} {
		p := &capacityEmbedding{capacity: capacity}
		svc, err := NewService(Models{"chat": stubChatProvider{}}, Aliases{}, testPolicy, discardLogger(), WithEmbeddings(EmbeddingModels{"embed": p}))
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 5; i++ {
			_, err = svc.Embed(context.Background(), &llmtypes.EmbeddingRequest{Model: "embed", Input: []byte(`"text"`)})
			if err == nil {
				t.Fatal("expected rejection")
			}
		}
		if svc.breakers.isOpen("embed") == capacity {
			t.Fatalf("capacity=%v: wrong breaker state", capacity)
		}
		if capacity && p.calls != 5 {
			t.Fatal("healthy worker became unreachable")
		}
	}
}
