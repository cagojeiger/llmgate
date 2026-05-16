package llmrouter

import (
	"context"
	"errors"
	"testing"

	"llmgate/internal/llmtypes"
	"llmgate/internal/providers/fake"
)

func TestService_StreamAliasFallback_RetriesOnEligiblePreStreamError(t *testing.T) {
	openAI := fake.NewProvider("openai", fake.WithStreamErrorOnModel(
		"deepseek-v4-pro",
		&llmtypes.Error{Kind: llmtypes.KindRateLimit, Message: "stream throttled", StatusCode: 429},
	))
	svc := mustService(t, fallbackCatalog(t), openAI, nil)

	result, err := svc.CompleteStream(context.Background(), chatRequest("coder", "hi"))
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	if result.Stream == nil {
		t.Fatal("CompleteStream: result.Stream is nil")
	}
	if result.ModelUsed != "deepseek-v4-flash" {
		t.Errorf("ModelUsed = %q, want deepseek-v4-flash", result.ModelUsed)
	}
	if openAI.StreamCalls() != 2 || openAI.LastStreamRequest().Model != "deepseek-v4-flash" {
		t.Fatalf(
			"stream calls/model = %d/%q, want 2/deepseek-v4-flash",
			openAI.StreamCalls(),
			openAI.LastStreamRequest().Model,
		)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(result.Attempts))
	}
	if result.Attempts[0].Model != "deepseek-v4-pro" || result.Attempts[0].Kind != llmtypes.KindRateLimit {
		t.Errorf("attempt[0] = %+v, want deepseek-v4-pro rate_limit", result.Attempts[0])
	}
	if result.Attempts[1].Model != "deepseek-v4-flash" || result.Attempts[1].Kind != "" {
		t.Errorf("attempt[1] = %+v, want deepseek-v4-flash success", result.Attempts[1])
	}
}

func TestService_StreamAliasFallback_BadRequestStopsImmediately(t *testing.T) {
	openAI := fake.NewProvider("openai", fake.WithStreamErrorOnModel(
		"deepseek-v4-pro",
		&llmtypes.Error{Kind: llmtypes.KindBadRequest, Message: "malformed stream"},
	))
	svc := mustService(t, fallbackCatalog(t), openAI, nil)

	result, err := svc.CompleteStream(context.Background(), chatRequest("coder", "hi"))
	if err == nil {
		t.Fatal("CompleteStream: want error")
	}
	var perr *llmtypes.Error
	if !errors.As(err, &perr) || perr.Kind != llmtypes.KindBadRequest {
		t.Fatalf("err = %v, want KindBadRequest", err)
	}
	if openAI.StreamCalls() != 1 {
		t.Errorf("streamCalls = %d, want 1 (no fallback for non-eligible)", openAI.StreamCalls())
	}
	if len(result.Attempts) != 1 || result.Attempts[0].Kind != llmtypes.KindBadRequest {
		t.Fatalf("attempts = %+v, want one bad_request attempt", result.Attempts)
	}
}

func TestService_StreamSkipsOpenCircuitModel(t *testing.T) {
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

	result, err := svc.CompleteStream(context.Background(), chatRequest("coder", "x"))
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	if result.ModelUsed != "deepseek-v4-flash" {
		t.Fatalf("ModelUsed = %q, want deepseek-v4-flash (primary circuit open)", result.ModelUsed)
	}
	if openAI.StreamCalls() != 1 || openAI.LastStreamRequest().Model != "deepseek-v4-flash" {
		t.Fatalf(
			"stream calls/model = %d/%q, want 1/deepseek-v4-flash",
			openAI.StreamCalls(),
			openAI.LastStreamRequest().Model,
		)
	}
	if len(result.Attempts) != 1 || result.Attempts[0].Model != "deepseek-v4-flash" {
		t.Fatalf("attempts = %+v, want one flash attempt", result.Attempts)
	}
}

func TestService_StreamPreStreamFailuresOpenCircuit(t *testing.T) {
	openAI := fake.NewProvider("openai", fake.WithStreamErrorOnModel(
		"deepseek-v4-pro",
		&llmtypes.Error{Kind: llmtypes.KindUpstream, Message: "stream setup failed"},
	))
	svc := mustService(t, fallbackCatalog(t), openAI, nil)

	for i := 0; i < 3; i++ {
		result, err := svc.CompleteStream(context.Background(), chatRequest("coder", "x"))
		if err != nil {
			t.Fatalf("run %d: unexpected error %v", i, err)
		}
		if result.ModelUsed != "deepseek-v4-flash" {
			t.Fatalf("run %d ModelUsed = %q, want deepseek-v4-flash", i, result.ModelUsed)
		}
	}
	if openAI.StreamCalls() != 6 {
		t.Fatalf("after 3 runs streamCalls = %d, want 6", openAI.StreamCalls())
	}

	beforeSkip := openAI.StreamCalls()
	result, err := svc.CompleteStream(context.Background(), chatRequest("coder", "x"))
	if err != nil {
		t.Fatalf("fourth CompleteStream: %v", err)
	}
	if added := openAI.StreamCalls() - beforeSkip; added != 1 {
		t.Fatalf("fourth run added %d stream calls, want 1 (primary skipped)", added)
	}
	if result.ModelUsed != "deepseek-v4-flash" {
		t.Fatalf("fourth ModelUsed = %q, want deepseek-v4-flash", result.ModelUsed)
	}
}

func TestService_StreamEmptyFirstEventFallsBack(t *testing.T) {
	openAI := fake.NewProvider("openai", fake.WithStreamEmptyEOFOnModel("deepseek-v4-pro"))
	svc := mustService(t, fallbackCatalog(t), openAI, nil)

	result, err := svc.CompleteStream(context.Background(), chatRequest("coder", "x"))
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	if result.ModelUsed != "deepseek-v4-flash" {
		t.Fatalf("ModelUsed = %q, want deepseek-v4-flash after primary empty stream", result.ModelUsed)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(result.Attempts))
	}
	if result.Attempts[0].Kind != llmtypes.KindUpstream {
		t.Fatalf("attempt[0].Kind = %q, want upstream", result.Attempts[0].Kind)
	}
}
