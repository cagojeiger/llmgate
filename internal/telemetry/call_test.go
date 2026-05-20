package telemetry

import (
	"testing"

	"llmgate/internal/domain/llmtypes"
)

func TestCallAttemptHelpers(t *testing.T) {
	call := &CallEvent{Attempts: []llmtypes.Attempt{
		{Vendor: "opencode", Model: "a", StatusCode: 429},
		{Vendor: "opencode", Model: "b", StatusCode: 200},
	}}
	if AttemptsCount(call) != 2 {
		t.Fatalf("AttemptsCount = %d, want 2", AttemptsCount(call))
	}
	final, ok := FinalAttempt(call)
	if !ok || final.Model != "b" || final.StatusCode != 200 {
		t.Fatalf("FinalAttempt = %+v/%v, want model b status 200", final, ok)
	}
}
