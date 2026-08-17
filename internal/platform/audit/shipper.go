package audit

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ObjectStore is the upload boundary. Keeping the shipper behind this
// interface means the rotation/retention logic is exercised in unit
// tests with an in-memory fake and never needs a live S3/MinIO.
type ObjectStore interface {
	Put(ctx context.Context, key, filePath string) error
}

// shipper drains pending/ to the store and reaps uploaded/ by age. It
// owns no writer state; the FileSink hands it the three directories and
// runs it as a background goroutine. All operations are best-effort:
// failures are logged and retried on the next tick, never surfaced to
// the request path.
type shipper struct {
	activeDir   string
	pendingDir  string
	uploadedDir string

	store  ObjectStore // nil => local-only rolling log
	prefix string
	cfg    Config
	log    *slog.Logger
}

func newShipper(dirs dirs, store ObjectStore, prefix string, cfg Config, log *slog.Logger) *shipper {
	return &shipper{
		activeDir:   dirs.active,
		pendingDir:  dirs.pending,
		uploadedDir: dirs.uploaded,
		store:       store,
		prefix:      prefix,
		cfg:         cfg,
		log:         log,
	}
}

// run ticks a single maintenance loop until ctx is cancelled. One loop
// (rather than separate upload/reap goroutines) keeps the passes ordered
// — upload before reap — so retention never races an in-flight upload.
func (s *shipper) run(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.UploadInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.pass(ctx)
		}
	}
}

// flush runs one synchronous pass. FileSink.Close calls it after sealing
// the active file so shutdown ships whatever it can within the close
// budget; anything left behind is picked up on the next boot.
func (s *shipper) flush(ctx context.Context) {
	s.pass(ctx)
}

func (s *shipper) pass(ctx context.Context) {
	if s.store != nil {
		s.uploadPass(ctx)
	}
	s.reapPass()
}

// uploadPass pushes every sealed file to the store, moving each to
// uploaded/ on success. Keys are derived from the seal time encoded in
// the filename so a retry after a crash re-puts the identical key.
func (s *shipper) uploadPass(ctx context.Context) {
	files := listFiles(s.pendingDir)
	for _, f := range files {
		if ctx.Err() != nil {
			return
		}
		key := objectKey(s.prefix, sealTimeOf(f), f.name)
		if err := s.store.Put(ctx, key, f.path); err != nil {
			s.log.LogAttrs(ctx, slog.LevelWarn, "audit upload failed",
				slog.String("file", f.name), slog.String("key", key), slog.String("err", err.Error()))
			continue // leave in pending/, retry next tick
		}
		dst := filepath.Join(s.uploadedDir, f.name)
		if err := os.Rename(f.path, dst); err != nil {
			s.log.LogAttrs(ctx, slog.LevelWarn, "audit mark-uploaded failed",
				slog.String("file", f.name), slog.String("err", err.Error()))
		}
	}
}

// reapPass enforces retention then the disk cap. With a store, retention
// deletes uploaded/ files older than Retention (they are safe in object
// storage). Without a store the local copy is authoritative, so
// retention applies to pending/ by seal age instead.
func (s *shipper) reapPass() {
	retainDir := s.uploadedDir
	if s.store == nil {
		retainDir = s.pendingDir
	}
	cutoff := time.Now().Add(-s.cfg.Retention)
	for _, f := range listFiles(retainDir) {
		if sealTimeOf(f).Before(cutoff) {
			s.remove(f.path)
		}
	}
	s.enforceDiskCap()
}

// enforceDiskCap keeps the on-disk footprint under DiskCap by dropping
// already-uploaded files first (no data loss) and only then oldest
// pending files (bounded loss, accepted for this data). active/ is never
// touched — the writer owns it.
func (s *shipper) enforceDiskCap() {
	if s.cfg.DiskCap <= 0 {
		return
	}
	active := listFiles(s.activeDir)
	uploaded := listFiles(s.uploadedDir)
	pending := listFiles(s.pendingDir)
	total := sumSize(active) + sumSize(uploaded) + sumSize(pending)
	if total <= s.cfg.DiskCap {
		return
	}
	// Oldest-first within each tier; uploaded is drained before pending.
	sortBySealTime(uploaded)
	sortBySealTime(pending)
	for _, tier := range [][]fileInfo{uploaded, pending} {
		for _, f := range tier {
			if total <= s.cfg.DiskCap {
				return
			}
			if s.remove(f.path) {
				total -= f.size
			}
		}
	}
}

func (s *shipper) remove(path string) bool {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		s.log.LogAttrs(context.Background(), slog.LevelWarn, "audit reap failed",
			slog.String("path", path), slog.String("err", err.Error()))
		return false
	}
	return true
}

type fileInfo struct {
	name string
	path string
	size int64
	mod  time.Time
}

func listFiles(dir string) []fileInfo {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]fileInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, fileInfo{name: e.Name(), path: filepath.Join(dir, e.Name()), size: info.Size(), mod: info.ModTime()})
	}
	return out
}

func sumSize(files []fileInfo) int64 {
	var n int64
	for _, f := range files {
		n += f.size
	}
	return n
}

func sortBySealTime(files []fileInfo) {
	sort.Slice(files, func(i, j int) bool { return sealTimeOf(files[i]).Before(sealTimeOf(files[j])) })
}

// sealTimeOf recovers the authoritative seal instant from the filename
// (<instance>-<20060102T150405Z>-<rand>.jsonl), falling back to the file
// mod time if the name is not one we wrote.
func sealTimeOf(f fileInfo) time.Time {
	if t, ok := parseSealTime(f.name); ok {
		return t
	}
	return f.mod
}
