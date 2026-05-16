package llmrouter

import (
	"context"
	"testing"

	"llmgate/internal/llmtypes"
	"llmgate/internal/providers/fake"
)

func TestService_CircuitOpensAfterRepeatedFailures(t *testing.T) {
	// Only the primary fails — secondary always succeeds. Three failed
	// runs trip the breaker on the primary; the fourth call must skip
	// the primary and hit secondary directly.
	openAI := fake.NewProvider("openai", fake.WithCompleteErrorOnModel(
		"deepseek-v4-pro",
		&llmtypes.Error{Kind: llmtypes.KindUpstream, Message: "boom"},
	))
	svc := mustService(t, fallbackCatalog(t), openAI, nil)

	for i := 0; i < 3; i++ {
		_, err := svc.Complete(context.Background(), chatRequest("coder", "x"))
		if err != nil {
			t.Fatalf("run %d: unexpected error %v", i, err)
		}
	}
	// 3 runs × 2 calls (pro fail + flash success) = 6 calls.
	if openAI.CompleteCalls() != 6 {
		t.Fatalf("after 3 runs completeCalls = %d, want 6", openAI.CompleteCalls())
	}

	// Fourth run: primary breaker is open → only flash is called (1 call).
	beforeSkip := openAI.CompleteCalls()
	_, _ = svc.Complete(context.Background(), chatRequest("coder", "x"))
	added := openAI.CompleteCalls() - beforeSkip
	if added != 1 {
		t.Errorf("fourth run added %d calls, want 1 (primary skipped)", added)
	}
}
