// Package audit implements a local-rotate + best-effort-upload sink for
// finalized LLM result events. The service writes newline-delimited JSON
// to a local file, seals it on a time/size cadence, and a background
// shipper uploads sealed files to an S3-compatible store and reaps the
// local copies by age. It deliberately depends on no message broker or
// log-shipping agent — only object storage (a library, not a deployed
// component) — trading a bounded crash-loss window for that simplicity.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	result "llmgate/internal/domain/llmresult/schema"
)

// finalFlushTimeout bounds the synchronous upload attempt on Close so a
// dead store cannot stall shutdown past the operator's grace period.
const finalFlushTimeout = 15 * time.Second

type dirs struct {
	active     string
	pending    string
	compressed string
	uploaded   string
}

// FileSink is the terminal Sink. It is a thin composition: the writer
// produces sealed files, the shipper uploads and reaps them, and FileSink
// wires the two, runs their background loops, and sequences shutdown. It
// is meant to be wrapped by the shared AsyncSink so writes stay off the
// request path.
type FileSink struct {
	dirs    dirs
	w       *writer
	shipper *shipper
	log     *slog.Logger

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewFileSink prepares the working directories, recovers any file left
// active by a previous crash, and starts the rotation + shipper loops.
// store may be nil for a local-only rolling log.
func NewFileSink(cfg Config, store ObjectStore, prefix string, log *slog.Logger) (*FileSink, error) {
	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	d := dirs{
		active:     filepath.Join(cfg.Dir, "active"),
		pending:    filepath.Join(cfg.Dir, "pending"),
		compressed: filepath.Join(cfg.Dir, "compressed"),
		uploaded:   filepath.Join(cfg.Dir, "uploaded"),
	}
	for _, p := range []string{d.active, d.pending, d.compressed, d.uploaded} {
		if err := os.MkdirAll(p, 0o750); err != nil {
			return nil, fmt.Errorf("audit: mkdir %s: %w", p, err)
		}
	}

	w := newWriter(d, instanceID(), cfg.RotateMaxBytes, cfg.RotateInterval, log)
	w.recoverOrphans()

	s := &FileSink{
		dirs:    d,
		w:       w,
		shipper: newShipper(d, store, prefix, cfg, log),
		log:     log,
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.wg.Add(2)
	go func() { defer s.wg.Done(); s.rotateLoop(ctx, cfg.RotateInterval) }()
	go func() { defer s.wg.Done(); s.shipper.run(ctx) }()
	return s, nil
}

// Emit encodes one event as a JSON line and hands it to the writer. It is
// called by the AsyncSink worker, off the request path. Failures are
// logged, never returned — the audit path must not affect request
// handling.
func (s *FileSink) Emit(ctx context.Context, event *result.Event) {
	if s == nil || event == nil {
		return
	}
	line, err := json.Marshal(event)
	if err != nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "audit marshal failed", slog.String("err", err.Error()))
		return
	}
	s.w.appendLine(ctx, append(line, '\n'))
}

// rotateLoop seals the active file at each clock boundary aligned to
// interval (e.g. hourly at :00, or every 10m at :00/:10/…) so each sealed
// file maps to a clean time bucket. Aligning to the wall clock — rather
// than interval-after-start — is what makes a file's coverage obvious
// when analyzing later.
func (s *FileSink) rotateLoop(ctx context.Context, interval time.Duration) {
	for {
		timer := time.NewTimer(time.Until(nextBoundary(time.Now(), interval)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			s.w.rotate()
		}
	}
}

// nextBoundary returns the next wall-clock instant that is a whole
// multiple of interval (in UTC), so buckets align across replicas.
func nextBoundary(now time.Time, interval time.Duration) time.Time {
	return now.UTC().Truncate(interval).Add(interval)
}

// Close stops the loops, seals the final active file, then makes one
// bounded synchronous shipping pass so shutdown uploads what it can.
// Anything still local is picked up on the next boot.
func (s *FileSink) Close() error {
	if s == nil {
		return nil
	}
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait() // loops stopped: no concurrent rotate/pass

	s.w.close()

	ctx, cancel := context.WithTimeout(context.Background(), finalFlushTimeout)
	defer cancel()
	s.shipper.flush(ctx)
	return nil
}
