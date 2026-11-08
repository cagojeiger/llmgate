package audit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const repairSuffix = ".repair"

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
	rotateInterval time.Duration
	log            *slog.Logger

	mu sync.Mutex
	f  *os.File
	// activeName is the final sealed name, fixed when the file opens:
	// <instance>-<bucketStart>-<rand>.jsonl, where bucketStart is the
	// open time truncated to rotateInterval (the DATA window's start, not
	// the seal instant — a file covering 10:00–11:00 keys under hour=10).
	// Naming at open makes the active file instance-scoped and unique, so
	// a failed seal-rename can never make the next write append across
	// buckets, and two processes sharing a dir never collide.
	activeName string
	size       int64
}

func newWriter(dir dirs, instance string, rotateMaxBytes int64, rotateInterval time.Duration, log *slog.Logger) *writer {
	return &writer{dir: dir, instance: instance, rotateMaxBytes: rotateMaxBytes, rotateInterval: rotateInterval, log: log}
}

// bucketStartOf returns the start of the UTC time bucket containing t —
// the wall-clock boundary aligned to interval, at or before t. All audit
// time labelling is UTC so buckets and partitions are unambiguous across
// regions and replicas.
func bucketStartOf(t time.Time, interval time.Duration) time.Time {
	if interval <= 0 {
		return t.UTC()
	}
	return t.UTC().Truncate(interval)
}

// recoverOrphans seals any file the previous process left in active/ (a
// crash before rotation) so its records ship rather than being appended
// to by this process. Runs before any goroutine starts, so it needs no
// lock. Orphans already carry their open-time bucket in their name, so we
// just move them as-is (no re-labelling); empty orphans (opened, never
// written, then crashed) are dropped to avoid a zero-record object.
func (w *writer) recoverOrphans() {
	var recovered, droppedEmpty int
	for _, f := range listFiles(w.dir.active) {
		if strings.HasSuffix(f.name, repairSuffix) {
			continue
		}
		if f.size == 0 {
			_ = os.Remove(f.path)
			droppedEmpty++
			continue
		}
		if err := os.Rename(f.path, filepath.Join(w.dir.pending, f.name)); err != nil {
			w.log.Warn("audit recover orphan failed", slog.String("file", f.name), slog.String("err", err.Error()))
			continue
		}
		recovered++
	}
	if recovered+droppedEmpty > 0 {
		w.log.Info("audit recovered orphaned files on boot",
			slog.Int("recovered", recovered), slog.Int("dropped_empty", droppedEmpty))
	}
}

// appendLine writes one already-encoded JSON line (newline included). On
// a short/failed write it seals the possibly-truncated file immediately
// so the damage is bounded to that one sealed file's tail and subsequent
// events start clean, rather than interleaving good lines after a partial
// one in the same growing file.
func (w *writer) appendLine(ctx context.Context, line []byte) {
	_, _ = w.append(ctx, line, false)
}

// An acknowledged append syncs before releasing the writer lock. The file
// can then rotate without changing the name returned to the caller.
func (w *writer) append(ctx context.Context, line []byte, acknowledged bool) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ensureOpenLocked(); err != nil {
		w.log.LogAttrs(ctx, slog.LevelWarn, "audit open failed", slog.String("err", err.Error()))
		return "", fmt.Errorf("open audit file: %w", err)
	}
	before := w.size
	n, err := w.f.Write(line)
	w.size += int64(n)
	if err == nil && n != len(line) {
		err = io.ErrShortWrite
	}
	if err != nil {
		if acknowledged {
			return "", w.recoverAppendLocked(ctx, before, fmt.Errorf("write audit line: %w", err))
		}
		w.log.LogAttrs(ctx, slog.LevelWarn, "audit write failed; sealing to contain partial line",
			slog.Int("wrote", n), slog.String("err", err.Error()))
		w.rotateLocked()
		return "", err
	}
	if acknowledged {
		if err := w.f.Sync(); err != nil {
			return "", w.recoverAppendLocked(ctx, before, fmt.Errorf("sync audit line: %w", err))
		}
	}
	name := w.activeName
	if w.rotateMaxBytes > 0 && w.size >= w.rotateMaxBytes {
		w.rotateLocked()
	}
	return name, nil
}

// Undo only the failed append. If the filesystem refuses truncation, keep
// the file for repair and do not let the shipper or orphan recovery publish
// a possibly complete-looking, unacknowledged result.
func (w *writer) recoverAppendLocked(ctx context.Context, before int64, cause error) error {
	if w.size > before {
		if err := w.f.Truncate(before); err != nil {
			f := w.f
			w.f, w.size, w.activeName = nil, 0, ""
			closeErr := f.Close()
			repairErr := os.Rename(f.Name(), f.Name()+repairSuffix)
			w.log.LogAttrs(ctx, slog.LevelError, "audit file requires repair",
				slog.String("file", filepath.Base(f.Name())),
				slog.Any("truncate_err", err), slog.Any("close_err", closeErr), slog.Any("rename_err", repairErr))
			return errors.Join(cause, err, closeErr, repairErr)
		}
		w.size = before
	}
	w.rotateLocked()
	return cause
}

func (w *writer) ensureOpenLocked() error {
	if w.f != nil {
		return nil
	}
	// Fix the bucket and the final name at open: the file that opens at
	// 10:00 and seals at the 11:00 boundary is labelled hour=10 (its data
	// window), and its unique instance-scoped name is set now so a later
	// seal-rename failure can never mix buckets.
	bucketStart := bucketStartOf(time.Now(), w.rotateInterval)
	w.activeName = sealedName(w.instance, bucketStart)
	f, err := os.OpenFile(filepath.Join(w.dir.active, w.activeName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
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
	name := w.activeName
	w.f, w.size, w.activeName = nil, 0, ""
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
	// The active file already has its final sealed name, so this is a
	// pure move. If it fails the file stays in active/ under that unique
	// name — the next write opens a fresh file (no bucket mixing) and
	// recoverOrphans re-seals the leftover on the next boot.
	if err := os.Rename(f.Name(), filepath.Join(w.dir.pending, name)); err != nil {
		w.log.Warn("audit seal rename failed; leftover kept for boot recovery", slog.String("file", name), slog.String("err", err.Error()))
	}
}

// fileEmpty reports whether f currently has zero bytes on disk.
func fileEmpty(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Size() == 0
}
