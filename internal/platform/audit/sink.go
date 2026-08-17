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

const activeFileName = "current.jsonl"

type dirs struct {
	active   string
	pending  string
	uploaded string
}

// FileSink is the terminal Sink: it appends events to the active file
// and owns the rotation and shipper goroutines. It is meant to be
// wrapped by the shared AsyncSink so writes stay off the request path.
type FileSink struct {
	dirs     dirs
	instance string
	cfg      Config
	log      *slog.Logger

	mu     sync.Mutex
	f      *os.File
	size   int64
	opened time.Time

	shipper *shipper
	cancel  context.CancelFunc
	wg      sync.WaitGroup
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
		active:   filepath.Join(cfg.Dir, "active"),
		pending:  filepath.Join(cfg.Dir, "pending"),
		uploaded: filepath.Join(cfg.Dir, "uploaded"),
	}
	for _, p := range []string{d.active, d.pending, d.uploaded} {
		if err := os.MkdirAll(p, 0o750); err != nil {
			return nil, fmt.Errorf("audit: mkdir %s: %w", p, err)
		}
	}

	s := &FileSink{
		dirs:     d,
		instance: instanceID(),
		cfg:      cfg,
		log:      log,
		shipper:  newShipper(d, store, prefix, cfg, log),
	}
	s.recoverOrphans()

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.wg.Add(2)
	go func() { defer s.wg.Done(); s.rotateLoop(ctx) }()
	go func() { defer s.wg.Done(); s.shipper.run(ctx) }()
	return s, nil
}

// recoverOrphans seals any file the previous process left in active/
// (a crash before rotation) so its records are shipped rather than
// overwritten. The seal time falls back to the file's mod time.
func (s *FileSink) recoverOrphans() {
	for _, f := range listFiles(s.dirs.active) {
		name := sealedName(s.instance, f.mod)
		if err := os.Rename(f.path, filepath.Join(s.dirs.pending, name)); err != nil {
			s.log.Warn("audit recover orphan failed", slog.String("file", f.name), slog.String("err", err.Error()))
		}
	}
}

// Emit appends one event as a JSON line. It is called by the AsyncSink
// worker, off the request path. Write failures are logged, never
// returned — the audit path must not affect request handling.
func (s *FileSink) Emit(ctx context.Context, event *result.Event) {
	if s == nil || event == nil {
		return
	}
	line, err := json.Marshal(event)
	if err != nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "audit marshal failed", slog.String("err", err.Error()))
		return
	}
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked(); err != nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "audit open failed", slog.String("err", err.Error()))
		return
	}
	n, err := s.f.Write(line)
	if err != nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "audit write failed", slog.String("err", err.Error()))
		return
	}
	s.size += int64(n)
	if s.cfg.RotateMaxBytes > 0 && s.size >= s.cfg.RotateMaxBytes {
		s.rotateLocked()
	}
}

func (s *FileSink) ensureOpenLocked() error {
	if s.f != nil {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(s.dirs.active, activeFileName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	s.f, s.size, s.opened = f, 0, time.Now()
	return nil
}

// rotateLocked seals the active file into pending/ under a fresh sealed
// name. Empty files are skipped so upload never creates zero-byte
// objects. fsync before rename makes the sealed bytes durable on disk
// before the shipper can pick the file up. Caller holds s.mu.
func (s *FileSink) rotateLocked() {
	if s.f == nil {
		return
	}
	f := s.f
	size := s.size
	s.f, s.size = nil, 0
	if size == 0 && fileEmpty(f) {
		// nothing written since last rotation; drop the empty active file
		_ = f.Close()
		_ = os.Remove(f.Name())
		return
	}
	if err := f.Sync(); err != nil {
		s.log.Warn("audit fsync failed", slog.String("err", err.Error()))
	}
	if err := f.Close(); err != nil {
		s.log.Warn("audit close active failed", slog.String("err", err.Error()))
	}
	name := sealedName(s.instance, time.Now())
	if err := os.Rename(f.Name(), filepath.Join(s.dirs.pending, name)); err != nil {
		s.log.Warn("audit seal rename failed", slog.String("err", err.Error()))
	}
}

func (s *FileSink) rotateLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.RotateInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			s.rotateLocked()
			s.mu.Unlock()
		}
	}
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

	s.mu.Lock()
	s.rotateLocked()
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), finalFlushTimeout)
	defer cancel()
	s.shipper.flush(ctx)
	return nil
}

// fileEmpty reports whether f currently has zero bytes on disk. Used to
// distinguish a freshly (re)opened active file from one with records.
func fileEmpty(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Size() == 0
}
