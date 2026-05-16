package llmrouter

import (
	"context"
	"errors"
	"testing"

	"llmgate/internal/llmtypes"
	"llmgate/internal/providers/fake"
)

func TestService_AliasFallback_PrimarySucceeds(t *testing.T) {
	openAI := fake.NewProvider("openai")
	svc := mustService(t, fallbackCatalog(t), openAI, nil)

	result, err := svc.Complete(context.Background(), chatRequest("coder", "hi"))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if result.Response == nil || result.Response.Model != "deepseek-v4-pro" {
		t.Errorf("result.Response.Model = %v, want deepseek-v4-pro", result.Response)
	}
	if result.Vendor != "openai" || result.ModelUsed != "deepseek-v4-pro" {
		t.Errorf("Vendor/ModelUsed = %q/%q, want openai/deepseek-v4-pro", result.Vendor, result.ModelUsed)
	}
	if openAI.CompleteCalls() != 1 {
		t.Errorf("completeCalls = %d, want 1 (no fallback needed)", openAI.CompleteCalls())
	}
	if len(result.Attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(result.Attempts))
	}
	if result.Attempts[0].Model != "deepseek-v4-pro" || result.Attempts[0].Vendor != "openai" {
		t.Errorf("attempt[0] = %+v, want deepseek-v4-pro / openai", result.Attempts[0])
	}
}

func TestService_AliasFallback_RetriesOnEligibleError(t *testing.T) {
	// Primary fails with KindRateLimit (eligible) → next chain entry tried.
	openAI := fake.NewProvider("openai", fake.WithCompleteErrorOnModel(
		"deepseek-v4-pro",
		&llmtypes.Error{Kind: llmtypes.KindRateLimit, Message: "throttled", StatusCode: 429},
	))
	svc := mustService(t, fallbackCatalog(t), openAI, nil)

	result, err := svc.Complete(context.Background(), chatRequest("coder", "hi"))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if result.Response == nil || result.Response.Model != "deepseek-v4-flash" {
		t.Errorf("result.Response.Model = %v, want deepseek-v4-flash (after fallback)", result.Response)
	}
	if result.ModelUsed != "deepseek-v4-flash" {
		t.Errorf("ModelUsed = %q, want deepseek-v4-flash", result.ModelUsed)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(result.Attempts))
	}
	if result.Attempts[0].Kind != llmtypes.KindRateLimit || result.Attempts[0].StatusCode != 429 {
		t.Errorf("attempt[0] = %+v, want rate_limit/429", result.Attempts[0])
	}
	if result.Attempts[1].Kind != "" || result.Attempts[1].StatusCode != 200 {
		t.Errorf("attempt[1] = %+v, want success", result.Attempts[1])
	}
}

func TestService_AliasFallback_BadRequestStopsImmediately(t *testing.T) {
	// Primary fails with KindBadRequest (not eligible) → return immediately.
	openAI := fake.NewProvider("openai", fake.WithCompleteErrorOnModel(
		"deepseek-v4-pro",
		&llmtypes.Error{Kind: llmtypes.KindBadRequest, Message: "malformed"},
	))
	svc := mustService(t, fallbackCatalog(t), openAI, nil)

	result, err := svc.Complete(context.Background(), chatRequest("coder", "hi"))
	if err == nil {
		t.Fatal("Complete: want error")
	}
	var perr *llmtypes.Error
	if !errors.As(err, &perr) || perr.Kind != llmtypes.KindBadRequest {
		t.Fatalf("err = %v, want KindBadRequest", err)
	}
	if openAI.CompleteCalls() != 1 {
		t.Errorf("completeCalls = %d, want 1 (no fallback for non-eligible)", openAI.CompleteCalls())
	}
	if len(result.Attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(result.Attempts))
	}
}

func TestService_AliasFallback_AllExhausted(t *testing.T) {
	openAI := fake.NewProvider("openai", fake.WithCompleteError(
		&llmtypes.Error{Kind: llmtypes.KindUpstream, Message: "boom", StatusCode: 502},
	))
	svc := mustService(t, fallbackCatalog(t), openAI, nil)

	result, err := svc.Complete(context.Background(), chatRequest("coder", "hi"))
	if err == nil {
		t.Fatal("Complete: want error")
	}
	var perr *llmtypes.Error
	if !errors.As(err, &perr) || perr.Kind != llmtypes.KindUpstream {
		t.Fatalf("err = %v, want KindUpstream (last attempt err)", err)
	}
	// chain has 4 openai-protocol entries; all should be tried before chain exhausted.
	if len(result.Attempts) != 4 {
		t.Fatalf("attempts = %d, want 4", len(result.Attempts))
	}
}
