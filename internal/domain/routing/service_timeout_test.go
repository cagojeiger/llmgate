package routing

import (
	"context"
	"errors"
	"testing"
	"time"

	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/llmtypes/fake"
)

func TestService_CompleteTimeoutFallsBack(t *testing.T) {
	openAI := fake.NewProvider("openai", fake.WithCompleteDelay("deepseek-v4-pro", 50*time.Millisecond))
	policy := testPolicy
	policy.CompleteTimeout = time.Millisecond
	svc := mustServiceWithPolicy(t, fallbackCatalog(t), openAI, nil, policy)

	result, err := svc.Complete(context.Background(), chatRequest("coder", "x"))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if result.ModelUsed != "deepseek-v4-flash" {
		t.Fatalf("ModelUsed = %q, want deepseek-v4-flash after primary timeout", result.ModelUsed)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(result.Attempts))
	}
	if result.Attempts[0].Kind != llmtypes.KindTimeout {
		t.Fatalf("attempt[0].Kind = %q, want timeout", result.Attempts[0].Kind)
	}
}

func TestService_RequestTimeoutStopsChain(t *testing.T) {
	openAI := fake.NewProvider("openai", fake.WithCompleteDelay("deepseek-v4-pro", 50*time.Millisecond))
	policy := testPolicy
	policy.CompleteTimeout = time.Minute
	svc := mustServiceWithPolicy(t, fallbackCatalog(t), openAI, nil, policy)

	// Request-level deadline lives on the caller's ctx (handler does this in
	// production); Service itself no longer adds a routeCtx wrap.
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	result, err := svc.Complete(ctx, chatRequest("coder", "x"))
	if err == nil {
		t.Fatal("Complete: want request timeout error")
	}
	var perr *llmtypes.Error
	if !errors.As(err, &perr) || perr.Kind != llmtypes.KindTimeout {
		t.Fatalf("err = %v, want provider timeout", err)
	}
	if openAI.CompleteCalls() != 1 {
		t.Fatalf("completeCalls = %d, want 1 (request budget exhausted before fallback)", openAI.CompleteCalls())
	}
	if len(result.Attempts) != 1 || result.Attempts[0].Kind != llmtypes.KindTimeout {
		t.Fatalf("attempts = %+v, want one timeout attempt", result.Attempts)
	}
}
