package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/streaming"
)

// hangingStream models a misbehaving upstream adapter: Recv blocks
// forever, Close does not unblock it. The receiver should give up
// after CloseGrace and log the abandon.
type hangingStream struct {
	closed chan struct{}
}

func newHangingStream() *hangingStream {
	return &hangingStream{closed: make(chan struct{})}
}

func (s *hangingStream) Recv() (*llmtypes.Event, error) {
	<-s.closed // Never closed by test; Recv never returns naturally.
	return nil, io.EOF
}

func (s *hangingStream) Close() error               { return nil }
func (s *hangingStream) Summary() *llmtypes.Summary { return &llmtypes.Summary{} }

func TestStreamReceiver_IdleTimeout_LogsAbandonWhenAdapterIgnoresClose(t *testing.T) {
	prev := streaming.CloseGrace
	streaming.CloseGrace = 20 * time.Millisecond
	defer func() { streaming.CloseGrace = prev }()

	buf := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(buf, nil))

	stream := newHangingStream()
	receiver := newStreamReceiver(stream, log)
	defer receiver.Stop()

	_, err := receiver.Recv(context.Background(), 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "stream idle timeout") {
		t.Fatalf("err = %v, want idle timeout", err)
	}

	// CloseGrace already elapsed inside Recv before it returned.
	logged := buf.String()
	if !strings.Contains(logged, "stream receiver recv abandoned") {
		t.Fatalf("abandon log missing; got=%s", logged)
	}

	var record map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
		if line == "" {
			continue
		}
		if err := json.Unmarshal([]byte(line), &record); err == nil {
			if record["msg"] == "stream receiver recv abandoned" {
				break
			}
		}
	}
	if record["trigger"] != "idle_timeout" {
		t.Errorf("trigger = %v, want idle_timeout", record["trigger"])
	}
	if record["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", record["level"])
	}
}

func TestStreamReceiver_CtxCancel_LogsAbandonWhenAdapterIgnoresClose(t *testing.T) {
	prev := streaming.CloseGrace
	streaming.CloseGrace = 20 * time.Millisecond
	defer func() { streaming.CloseGrace = prev }()

	buf := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(buf, nil))

	stream := newHangingStream()
	receiver := newStreamReceiver(stream, log)
	defer receiver.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	_, err := receiver.Recv(ctx, time.Hour)
	if err == nil {
		t.Fatal("err = nil, want non-nil from ctx cancel")
	}

	logged := buf.String()
	if !strings.Contains(logged, "stream receiver recv abandoned") {
		t.Fatalf("abandon log missing; got=%s", logged)
	}
	if !strings.Contains(logged, `"trigger":"ctx_cancelled"`) {
		t.Errorf("expected trigger=ctx_cancelled in log: %s", logged)
	}
}
