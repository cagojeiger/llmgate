package audit

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ObjectStore is the upload boundary. Keeping the shipper behind this
// interface means the compress/upload/reap logic is exercised in unit
// tests with an in-memory fake and never needs a live S3/MinIO.
type ObjectStore interface {
	Put(ctx context.Context, key, filePath string) error
}

// shipper advances sealed files through compress → upload → reap. It owns
// no writer state; the FileSink hands it the directories and runs it as a
// background goroutine. All operations are best-effort: failures are
// logged and retried on the next tick, never surfaced to the request
// path.
type shipper struct {
	activeDir     string
	pendingDir    string
	compressedDir string
	uploadedDir   string

	store  ObjectStore // nil => local-only rolling log
	prefix string
	cfg    Config
	log    *slog.Logger
}

func newShipper(dirs dirs, store ObjectStore, prefix string, cfg Config, log *slog.Logger) *shipper {
	return &shipper{
		activeDir:     dirs.active,
		pendingDir:    dirs.pending,
		compressedDir: dirs.compressed,
		uploadedDir:   dirs.uploaded,
		store:         store,
		prefix:        prefix,
		cfg:           cfg,
		log:           log,
	}
}

// run ticks a single maintenance loop until ctx is cancelled. One ordered
// loop (compress → upload → reap) keeps the stages from racing — nothing
// uploads a half-compressed file, and retention never races an in-flight
// upload. Keeping the tick short bounds the plaintext-on-disk window and
// enforces the disk cap frequently.
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
	s.compressPass(ctx)
	if s.store != nil {
		s.uploadPass(ctx)
	}
	s.reapPass(ctx)
}

// compressPass moves sealed files from pending/ to compressed/. With gzip
// it streams pending/<name> → compressed/<name>.gz and drops the
// plaintext; with CompressionNone it just renames. The atomic hand-off
// means the uploader never sees a half-compressed file.
//
// Deliberately single-threaded — do NOT parallelize this. Compression is
// CPU-bound, and this audit path is a best-effort background concern that
// must not steal request-serving cores; running it in the one shipper
// goroutine caps it at a single core. (Upload is parallelized instead
// because it is I/O-bound and barely touches the CPU.)
func (s *shipper) compressPass(ctx context.Context) {
	for _, f := range listFiles(s.pendingDir) {
		if ctx.Err() != nil {
			return
		}
		if s.cfg.Compression == CompressionNone {
			if err := os.Rename(f.path, filepath.Join(s.compressedDir, f.name)); err != nil {
				s.log.LogAttrs(ctx, slog.LevelWarn, "audit stage failed",
					slog.String("file", f.name), slog.String("err", err.Error()))
			}
			continue
		}
		dst := filepath.Join(s.compressedDir, f.name+".gz")
		if err := compressFile(f.path, dst); err != nil {
			s.log.LogAttrs(ctx, slog.LevelWarn, "audit compress failed",
				slog.String("file", f.name), slog.String("err", err.Error()))
			continue // leave in pending/, retry next tick
		}
		s.remove(ctx, f.path) // drop the plaintext original
	}
}

// uploadPass pushes every compressed file to the store, up to
// UploadConcurrency in parallel — a single sequential stream is the main
// per-replica throughput ceiling. Keys derive from the seal time encoded
// in the filename so a retry after a crash re-puts the identical key.
func (s *shipper) uploadPass(ctx context.Context) {
	files := listFiles(s.compressedDir)
	if len(files) == 0 {
		return
	}
	conc := s.cfg.UploadConcurrency
	if conc < 1 {
		conc = 1
	}
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for _, f := range files {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(f fileInfo) {
			defer wg.Done()
			defer func() { <-sem }()
			s.uploadOne(ctx, f)
		}(f)
	}
	wg.Wait()
}

func (s *shipper) uploadOne(ctx context.Context, f fileInfo) {
	key := objectKey(s.prefix, sealTimeOf(f), f.name)
	if err := s.store.Put(ctx, key, f.path); err != nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "audit upload failed",
			slog.String("file", f.name), slog.String("key", key), slog.String("err", err.Error()))
		return // leave in compressed/, retry next tick
	}
	if err := os.Rename(f.path, filepath.Join(s.uploadedDir, f.name)); err != nil {
		// Object is already in the store; drop the local copy rather than
		// re-uploading the same file on every tick.
		s.log.LogAttrs(ctx, slog.LevelWarn, "audit mark-uploaded failed; dropping local copy (already in store)",
			slog.String("file", f.name), slog.String("err", err.Error()))
		s.remove(ctx, f.path)
	}
}

// reapPass enforces retention then the disk cap. With a store, retention
// deletes uploaded/ files older than Retention (safe in object storage);
// in local-only mode the compressed/ copy is authoritative, so retention
// applies there by seal age instead.
func (s *shipper) reapPass(ctx context.Context) {
	retainDir := s.uploadedDir
	if s.store == nil {
		retainDir = s.compressedDir
	}
	cutoff := time.Now().Add(-s.cfg.Retention)
	for _, f := range listFiles(retainDir) {
		if sealTimeOf(f).Before(cutoff) {
			s.remove(ctx, f.path)
		}
	}
	s.enforceDiskCap(ctx)
}

// enforceDiskCap keeps the on-disk footprint under DiskCap by dropping
// already-uploaded files first (no data loss), then oldest compressed,
// then oldest pending (bounded loss, accepted for this data). active/ is
// never touched — the writer owns it.
func (s *shipper) enforceDiskCap(ctx context.Context) {
	if s.cfg.DiskCap <= 0 {
		return
	}
	active := listFiles(s.activeDir)
	uploaded := listFiles(s.uploadedDir)
	compressed := listFiles(s.compressedDir)
	pending := listFiles(s.pendingDir)
	total := sumSize(active) + sumSize(uploaded) + sumSize(compressed) + sumSize(pending)
	if total <= s.cfg.DiskCap {
		return
	}
	sortBySealTime(uploaded)
	sortBySealTime(compressed)
	sortBySealTime(pending)
	tiers := []struct {
		files []fileInfo
		lossy bool // not yet uploaded — dropping these loses data
	}{
		{uploaded, false},
		{compressed, true},
		{pending, true},
	}
	for _, tier := range tiers {
		for _, f := range tier.files {
			if total <= s.cfg.DiskCap {
				return
			}
			if tier.lossy {
				// Best-effort loss is accepted, but never silent.
				s.log.LogAttrs(ctx, slog.LevelWarn, "audit disk cap exceeded; dropping un-uploaded file (data loss)",
					slog.String("file", f.name), slog.Int64("bytes", f.size), slog.Int64("cap", s.cfg.DiskCap))
			}
			if s.remove(ctx, f.path) {
				total -= f.size
			}
		}
	}
}

func (s *shipper) remove(ctx context.Context, path string) bool {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		s.log.LogAttrs(ctx, slog.LevelWarn, "audit reap failed",
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
// (<instance>-<20060102T150405Z>-<rand>.jsonl[.gz]), falling back to the
// file mod time if the name is not one we wrote.
func sealTimeOf(f fileInfo) time.Time {
	if t, ok := parseSealTime(f.name); ok {
		return t
	}
	return f.mod
}
