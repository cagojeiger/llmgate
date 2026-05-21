package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"llmgate/internal/domain/llmtypes"
)

// panickingStream models a misbehaving vendor adapter whose Recv()
// panics. The streamReceiver worker goroutine must recover so a single
// upstream bug does not take the whole process down.
type panickingStream struct{}

func (panickingStream) Recv() (*llmtypes.Event, error) { panic("boom") }
func (panickingStream) Close() error                   { return nil }
func (panickingStream) Summary() *llmtypes.Summary     { return &llmtypes.Summary{} }

func TestStreamReceiver_RecoversFromAdapterPanic(t *testing.T) {
	buf := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(buf, nil))

	receiver := newStreamReceiver(panickingStream{}, log)
	defer receiver.Stop()

	_, err := receiver.Recv(context.Background(), 5*time.Second)
	if err == nil {
		t.Fatal("err = nil, want a panic-kind error so the caller does not hang")
	}
	var perr *llmtypes.Error
	if !errors.As(err, &perr) || perr.Kind != llmtypes.KindPanic {
		t.Fatalf("err = %v, want KindPanic llmtypes.Error", err)
	}
	if !strings.Contains(perr.Message, "boom") {
		t.Errorf("err message = %q, want it to mention the panic value", perr.Message)
	}

	// One ERROR-level log line should have been emitted with the stack
	// so operators see what crashed. The exact stack contents are
	// runtime-dependent, so just check the headline message + level.
	logged := buf.String()
	if !strings.Contains(logged, "stream receiver worker panic") {
		t.Fatalf("panic log missing; got=%s", logged)
	}
	var record map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
		if err := json.Unmarshal([]byte(line), &record); err == nil {
			if record["msg"] == "stream receiver worker panic" {
				break
			}
		}
	}
	if record["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", record["level"])
	}
}
