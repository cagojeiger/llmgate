package streaming

import (
	"context"
	"errors"
	"strings"
	"testing"

	"llmgate/internal/domain/llmtypes"
)

// panickingStream models a vendor adapter that panics during the
// first Recv(). ValidateStreamStart's per-call goroutine must recover
// so the bug does not take the whole process down — the caller
// should see a typed KindPanic error instead.
type panickingStartStream struct{}

func (panickingStartStream) Recv() (*llmtypes.Event, error) { panic("kaboom") }
func (panickingStartStream) Close() error                   { return nil }
func (panickingStartStream) Summary() *llmtypes.Summary     { return &llmtypes.Summary{} }

func TestValidateStreamStart_RecoversFromAdapterPanic(t *testing.T) {
	_, err := ValidateStreamStart(context.Background(), panickingStartStream{})
	if err == nil {
		t.Fatal("err = nil, want a panic-kind error so the caller does not hang")
	}
	var perr *llmtypes.Error
	if !errors.As(err, &perr) || perr.Kind != llmtypes.KindPanic {
		t.Fatalf("err = %v, want KindPanic llmtypes.Error", err)
	}
	if !strings.Contains(perr.Message, "kaboom") {
		t.Errorf("err message = %q, want it to mention the panic value", perr.Message)
	}
}
