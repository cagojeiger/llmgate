package audit

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const activeFileName = "current.jsonl"

// writer turns events into sealed files: it appends lines to the active
// file and rotates it (fsync + atomic rename into pending/) on a size
// trigger or on demand. It owns only the active-file state; the shipper
// owns everything from pending/ onward. Splitting this out of FileSink
// keeps one responsibility per type — producing sealed files vs.
// orchestrating their lifecycle.
type writer struct {
	dir            dirs
	instance       string
	rotateMaxBytes int64
	log            *slog.Logger

	mu   sync.Mutex
	f    *os.File
	size int64
}

func newWriter(dir dirs, instance string, rotateMaxBytes int64, log *slog.Logger) *writer {
	return &writer{dir: dir, instance: instance, rotateMaxBytes: rotateMaxBytes, log: log}
}

// recoverOrphans seals any file the previous process left in active/ (a
// crash before rotation) so its records ship rather than being appended
// to by this process. Runs before any goroutine starts, so it needs no
// lock. The seal time falls back to the file's mod time.
func (w *writer) recoverOrphans() {
	for _, f := range listFiles(w.dir.active) {
		name := sealedName(w.instance, f.mod)
		if err := os.Rename(f.path, filepath.Join(w.dir.pending, name)); err != nil {
			w.log.Warn("audit recover orphan failed", slog.String("file", f.name), slog.String("err", err.Error()))
		}
	}
}

// appendLine writes one already-encoded JSON line (newline included). On
// a short/failed write it seals the possibly-truncated file immediately
// so the damage is bounded to that one sealed file's tail and subsequent
// events start clean, rather than interleaving good lines after a partial
// one in the same growing file.
func (w *writer) appendLine(ctx context.Context, line []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ensureOpenLocked(); err != nil {
		w.log.LogAttrs(ctx, slog.LevelWarn, "audit open failed", slog.String("err", err.Error()))
		return
	}
	n, err := w.f.Write(line)
	w.size += int64(n)
	if err != nil {
		w.log.LogAttrs(ctx, slog.LevelWarn, "audit write failed; sealing to contain partial line",
			slog.Int("wrote", n), slog.String("err", err.Error()))
		w.rotateLocked()
		return
	}
	if w.rotateMaxBytes > 0 && w.size >= w.rotateMaxBytes {
		w.rotateLocked()
	}
}

func (w *writer) ensureOpenLocked() error {
	if w.f != nil {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(w.dir.active, activeFileName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	w.f, w.size = f, 0
	return nil
}

// rotate seals the active file on demand (the time-based trigger). Safe
// to call when nothing is open — it no-ops.
func (w *writer) rotate() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.rotateLocked()
}

// close seals the final active file. After it returns the writer holds no
// open file. Caller must ensure no concurrent appendLine/rotate.
func (w *writer) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.rotateLocked()
}

// rotateLocked seals the active file into pending/ under a fresh sealed
// name. Empty files are dropped so upload never creates zero-byte
// objects. fsync before rename makes the bytes durable before the shipper
// can pick the file up. Caller holds w.mu.
func (w *writer) rotateLocked() {
	if w.f == nil {
		return
	}
	f := w.f
	size := w.size
	w.f, w.size = nil, 0
	if size == 0 && fileEmpty(f) {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return
	}
	if err := f.Sync(); err != nil {
		w.log.Warn("audit fsync failed", slog.String("err", err.Error()))
	}
	if err := f.Close(); err != nil {
		w.log.Warn("audit close active failed", slog.String("err", err.Error()))
	}
	name := sealedName(w.instance, time.Now())
	if err := os.Rename(f.Name(), filepath.Join(w.dir.pending, name)); err != nil {
		w.log.Warn("audit seal rename failed", slog.String("err", err.Error()))
	}
}

// fileEmpty reports whether f currently has zero bytes on disk.
func fileEmpty(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Size() == 0
}
