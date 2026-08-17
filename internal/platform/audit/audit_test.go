package audit

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	result "llmgate/internal/domain/llmresult/schema"
)

func slogDiscard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type fakeStore struct {
	mu       sync.Mutex
	puts     map[string][]byte
	failOnce bool
}

func newFakeStore() *fakeStore { return &fakeStore{puts: map[string][]byte{}} }

func (f *fakeStore) Put(_ context.Context, key, filePath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOnce {
		f.failOnce = false
		return errors.New("simulated upload failure")
	}
	b, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}
	if strings.HasSuffix(key, ".gz") { // store decompressed so line counts work
		zr, e := gzip.NewReader(bytes.NewReader(b))
		if e != nil {
			return e
		}
		if b, err = io.ReadAll(zr); err != nil {
			return err
		}
		_ = zr.Close()
	}
	f.puts[key] = b
	return nil
}

func (f *fakeStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.puts)
}

func (f *fakeStore) totalLines() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, b := range f.puts {
		n += len(strings.Split(strings.TrimRight(string(b), "\n"), "\n"))
	}
	return n
}

func event(id string) *result.Event {
	return &result.Event{
		SchemaVersion: result.SchemaVersion,
		EventType:     result.EventType,
		RequestID:     id,
		Timestamp:     time.Now(),
	}
}

// Close seals the active file and runs a synchronous shipping pass, so a
// happy-path emit→upload is fully deterministic without waiting on the
// hour-scale background tickers.
func TestFileSink_EmitCloseUploads(t *testing.T) {
	dir := t.TempDir()
	store := newFakeStore()
	s, err := NewFileSink(Config{Dir: dir}, store, "audit", nil)
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		s.Emit(ctx, event("r"+string(rune('0'+i))))
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if store.count() != 1 {
		t.Fatalf("object count = %d, want 1 (single sealed file)", store.count())
	}
	if store.totalLines() != 5 {
		t.Fatalf("total lines = %d, want 5", store.totalLines())
	}
	// key is time-partitioned and prefixed
	for k := range store.puts {
		if !strings.HasPrefix(k, "audit/dt=") || !strings.Contains(k, "/hour=") {
			t.Fatalf("object key %q not partitioned under prefix", k)
		}
	}
	// local pending/uploaded drained/retained as expected
	if got := len(listFiles(s.dirs.pending)); got != 0 {
		t.Fatalf("pending files = %d, want 0 after upload", got)
	}
	if got := len(listFiles(s.dirs.uploaded)); got != 1 {
		t.Fatalf("uploaded files = %d, want 1 (retained locally)", got)
	}
}

func TestFileSink_SizeRotationProducesMultipleObjects(t *testing.T) {
	dir := t.TempDir()
	store := newFakeStore()
	// 1-byte cap forces a seal after every event.
	s, err := NewFileSink(Config{Dir: dir, RotateMaxBytes: 1}, store, "", nil)
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		s.Emit(ctx, event("r"+string(rune('0'+i))))
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if store.count() != 4 {
		t.Fatalf("object count = %d, want 4 (one per event)", store.count())
	}
	if store.totalLines() != 4 {
		t.Fatalf("total lines = %d, want 4", store.totalLines())
	}
}

func TestFileSink_UploadRetryLeavesPending(t *testing.T) {
	dir := t.TempDir()
	store := newFakeStore()
	store.failOnce = true
	s, err := NewFileSink(Config{Dir: dir}, store, "", nil)
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	ctx := context.Background()
	s.Emit(ctx, event("r1"))

	// First pass: seal, compress, then a failing upload leaves the file
	// staged in compressed/ (not pending/ — compression already ran).
	s.w.rotate()
	s.shipper.pass(ctx)
	if got := len(listFiles(s.dirs.compressed)); got != 1 {
		t.Fatalf("compressed after failed upload = %d, want 1", got)
	}
	if store.count() != 0 {
		t.Fatalf("store count after failed upload = %d, want 0", store.count())
	}
	// Second pass: upload succeeds.
	s.shipper.pass(ctx)
	if store.count() != 1 {
		t.Fatalf("store count after retry = %d, want 1", store.count())
	}
	_ = s.Close()
}

func TestFileSink_LocalOnlyNoStore(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFileSink(Config{Dir: dir}, nil, "", nil)
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	ctx := context.Background()
	s.Emit(ctx, event("r1"))
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Local-only: file is compressed (as .jsonl.gz) but not uploaded — it
	// comes to rest in compressed/, the terminal local state.
	if got := len(listFiles(s.dirs.compressed)); got != 1 {
		t.Fatalf("compressed files = %d, want 1 in local-only mode", got)
	}
}

func TestShipper_ReapRetention(t *testing.T) {
	dir := t.TempDir()
	d := dirs{active: filepath.Join(dir, "active"), pending: filepath.Join(dir, "pending"), uploaded: filepath.Join(dir, "uploaded")}
	for _, p := range []string{d.active, d.pending, d.uploaded} {
		mustMkdir(t, p)
	}
	// old (beyond retention) and fresh uploaded files, named with seal time.
	old := sealedName("inst", time.Now().Add(-48*time.Hour))
	fresh := sealedName("inst", time.Now())
	writeFile(t, filepath.Join(d.uploaded, old), "x")
	writeFile(t, filepath.Join(d.uploaded, fresh), "y")

	sh := newShipper(d, newFakeStore(), "", Config{Retention: 24 * time.Hour}.withDefaults(), slogDiscard())
	sh.reapPass(context.Background())

	if fileExists(filepath.Join(d.uploaded, old)) {
		t.Fatalf("old uploaded file should be reaped past retention")
	}
	if !fileExists(filepath.Join(d.uploaded, fresh)) {
		t.Fatalf("fresh uploaded file should survive retention")
	}
}

func TestShipper_DiskCapDropsOldestUploadedFirst(t *testing.T) {
	dir := t.TempDir()
	d := dirs{active: filepath.Join(dir, "active"), pending: filepath.Join(dir, "pending"), uploaded: filepath.Join(dir, "uploaded")}
	for _, p := range []string{d.active, d.pending, d.uploaded} {
		mustMkdir(t, p)
	}
	older := sealedName("inst", time.Now().Add(-2*time.Hour))
	newer := sealedName("inst", time.Now().Add(-1*time.Hour))
	pending := sealedName("inst", time.Now())
	writeFile(t, filepath.Join(d.uploaded, older), strings.Repeat("a", 100))
	writeFile(t, filepath.Join(d.uploaded, newer), strings.Repeat("b", 100))
	writeFile(t, filepath.Join(d.pending, pending), strings.Repeat("c", 100))

	// Cap 150 forces dropping the oldest uploaded (100) → total 200; still
	// over, drop newer uploaded (100) → 100; pending is spared last.
	sh := newShipper(d, newFakeStore(), "", Config{DiskCap: 150, Retention: 999 * time.Hour}.withDefaults(), slogDiscard())
	sh.enforceDiskCap(context.Background())

	if fileExists(filepath.Join(d.uploaded, older)) {
		t.Fatalf("oldest uploaded should be dropped first")
	}
	if !fileExists(filepath.Join(d.pending, pending)) {
		t.Fatalf("pending (not yet uploaded) should be dropped last, still present here")
	}
}

func TestNaming_SealTimeRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	name := sealedName("llmgate-7d9c8f6b5-x2k4p", now)
	got, ok := parseSealTime(name)
	if !ok {
		t.Fatalf("parseSealTime(%q) failed", name)
	}
	if !got.Equal(now) {
		t.Fatalf("seal time round-trip: got %v want %v", got, now)
	}
	key := objectKey("audit", now, name)
	if !strings.HasPrefix(key, "audit/dt=") {
		t.Fatalf("objectKey %q missing prefix/partition", key)
	}
}

func TestNaming_UniqueAcrossSameSecond(t *testing.T) {
	now := time.Now()
	a := sealedName("inst", now)
	b := sealedName("inst", now)
	if a == b {
		t.Fatalf("sealed names collided within same second: %q", a)
	}
}

func TestFileSink_RecoverOrphans(t *testing.T) {
	dir := t.TempDir()
	// simulate a crash: a file left in active/ by a previous process.
	activeDir := filepath.Join(dir, "active")
	mustMkdir(t, activeDir)
	writeFile(t, filepath.Join(activeDir, activeFileName), `{"request_id":"orphan"}`+"\n")

	s, err := NewFileSink(Config{Dir: dir}, nil, "", nil)
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	defer func() { _ = s.Close() }()

	if got := len(listFiles(s.dirs.active)); got != 0 {
		t.Fatalf("active files after recover = %d, want 0", got)
	}
	if got := len(listFiles(s.dirs.pending)); got != 1 {
		t.Fatalf("pending files after recover = %d, want 1 (orphan sealed)", got)
	}
}

func TestWriter_RotateSealsActiveAndSkipsEmpty(t *testing.T) {
	dir := t.TempDir()
	d := dirs{active: filepath.Join(dir, "active"), pending: filepath.Join(dir, "pending"), uploaded: filepath.Join(dir, "uploaded")}
	for _, p := range []string{d.active, d.pending, d.uploaded} {
		mustMkdir(t, p)
	}
	w := newWriter(d, "inst", 0, time.Hour, slogDiscard())
	w.appendLine(context.Background(), []byte(`{"a":1}`+"\n"))
	w.rotate()
	if got := len(listFiles(d.pending)); got != 1 {
		t.Fatalf("pending after rotate = %d, want 1", got)
	}
	w.rotate() // nothing open -> no empty object
	if got := len(listFiles(d.pending)); got != 1 {
		t.Fatalf("empty rotate should not add a file; pending = %d", got)
	}
}

func TestConfig_DiskCapBelowRotateRejected(t *testing.T) {
	_, err := NewFileSink(Config{Dir: t.TempDir(), RotateMaxBytes: 1000, DiskCap: 500}, nil, "", nil)
	if err == nil {
		t.Fatal("NewFileSink should reject DiskCap < RotateMaxBytes")
	}
}

func TestShipper_DiskCapDropsPendingDataLoss(t *testing.T) {
	dir := t.TempDir()
	d := dirs{active: filepath.Join(dir, "active"), pending: filepath.Join(dir, "pending"), uploaded: filepath.Join(dir, "uploaded")}
	for _, p := range []string{d.active, d.pending, d.uploaded} {
		mustMkdir(t, p)
	}
	// local-only (no store), two pending files over the cap.
	old := sealedName("inst", time.Now().Add(-2*time.Hour))
	newer := sealedName("inst", time.Now().Add(-1*time.Hour))
	writeFile(t, filepath.Join(d.pending, old), strings.Repeat("a", 100))
	writeFile(t, filepath.Join(d.pending, newer), strings.Repeat("b", 100))

	sh := newShipper(d, nil, "", Config{DiskCap: 150, Retention: 999 * time.Hour}.withDefaults(), slogDiscard())
	sh.enforceDiskCap(context.Background())

	if fileExists(filepath.Join(d.pending, old)) {
		t.Fatal("oldest un-uploaded pending should be dropped when over cap (bounded data loss)")
	}
	if !fileExists(filepath.Join(d.pending, newer)) {
		t.Fatal("newer pending should survive once under cap")
	}
}

func TestNextBoundary_ClockAligned(t *testing.T) {
	now := time.Date(2026, 8, 17, 10, 37, 12, 0, time.UTC)
	if got := nextBoundary(now, 10*time.Minute); !got.Equal(time.Date(2026, 8, 17, 10, 40, 0, 0, time.UTC)) {
		t.Fatalf("10m boundary = %v, want 10:40", got)
	}
	if got := nextBoundary(now, time.Hour); !got.Equal(time.Date(2026, 8, 17, 11, 0, 0, 0, time.UTC)) {
		t.Fatalf("1h boundary = %v, want 11:00", got)
	}
}

func TestFileSink_GzipObjectKeyAndContent(t *testing.T) {
	store := newFakeStore()
	s, err := NewFileSink(Config{Dir: t.TempDir()}, store, "audit", nil)
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	for i := 0; i < 3; i++ {
		s.Emit(context.Background(), event("r"+string(rune('0'+i))))
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if store.count() != 1 {
		t.Fatalf("object count = %d, want 1", store.count())
	}
	for k := range store.puts {
		if !strings.HasSuffix(k, ".jsonl.gz") {
			t.Fatalf("object key %q should end .jsonl.gz", k)
		}
	}
	if store.totalLines() != 3 { // fakeStore decompresses .gz
		t.Fatalf("decompressed lines = %d, want 3", store.totalLines())
	}
}

func TestShipper_CompressionNoneStages(t *testing.T) {
	d := newDirs(t)
	writeFile(t, filepath.Join(d.pending, sealedName("inst", time.Now())), `{"a":1}`+"\n")
	sh := newShipper(d, nil, "", Config{Compression: CompressionNone}.withDefaults(), slogDiscard())
	sh.compressPass(context.Background())
	files := listFiles(d.compressed)
	if len(files) != 1 || strings.HasSuffix(files[0].name, ".gz") {
		t.Fatalf("none mode should stage a plain .jsonl in compressed/, got %v", files)
	}
	if len(listFiles(d.pending)) != 0 {
		t.Fatalf("pending should be drained")
	}
}

func TestShipper_UploadPassParallel(t *testing.T) {
	d := newDirs(t)
	store := newFakeStore()
	for i := 0; i < 5; i++ {
		writeFile(t, filepath.Join(d.compressed, sealedName("inst", time.Now().Add(time.Duration(i)*time.Second))), "x")
	}
	sh := newShipper(d, store, "", Config{UploadConcurrency: 4}.withDefaults(), slogDiscard())
	sh.uploadPass(context.Background())
	if store.count() != 5 {
		t.Fatalf("uploaded objects = %d, want 5", store.count())
	}
	if len(listFiles(d.uploaded)) != 5 || len(listFiles(d.compressed)) != 0 {
		t.Fatalf("compressed should drain to uploaded: compressed=%d uploaded=%d",
			len(listFiles(d.compressed)), len(listFiles(d.uploaded)))
	}
}

func TestBucketStartOf_UTC(t *testing.T) {
	now := time.Date(2026, 8, 17, 10, 37, 12, 0, time.UTC)
	if got := bucketStartOf(now, 10*time.Minute); !got.Equal(time.Date(2026, 8, 17, 10, 30, 0, 0, time.UTC)) {
		t.Fatalf("10m bucket start = %v, want 10:30 UTC", got)
	}
	if got := bucketStartOf(now, time.Hour); !got.Equal(time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("1h bucket start = %v, want 10:00 UTC", got)
	}
	// a time in another zone is normalised to UTC before truncating
	kst := time.FixedZone("KST", 9*3600)
	got := bucketStartOf(time.Date(2026, 8, 17, 19, 37, 0, 0, kst), time.Hour) // 10:37 UTC
	if !got.Equal(time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("cross-zone bucket = %v, want 10:00 UTC", got)
	}
}

// A file that opens mid-bucket and seals at the next boundary must be
// labelled by the bucket START (its data window), not the seal instant.
func TestWriter_LabelsByBucketStart(t *testing.T) {
	d := newDirs(t)
	w := newWriter(d, "inst", 0, time.Hour, slogDiscard())
	w.appendLine(context.Background(), []byte(`{"a":1}`+"\n"))
	w.rotate()
	files := listFiles(d.pending)
	if len(files) != 1 {
		t.Fatalf("want 1 sealed file, got %d", len(files))
	}
	got, ok := parseSealTime(files[0].name)
	if !ok {
		t.Fatalf("parse seal time from %q failed", files[0].name)
	}
	if want := time.Now().UTC().Truncate(time.Hour); !got.Equal(want) {
		t.Fatalf("label = %v, want the bucket start %v (not the seal instant)", got, want)
	}
}

func TestConfig_RotateIntervalMustDivide24h(t *testing.T) {
	if _, err := NewFileSink(Config{Dir: t.TempDir(), RotateInterval: 7 * time.Minute}, nil, "", nil); err == nil {
		t.Fatal("7m must be rejected — it does not divide 24h evenly")
	}
	s, err := NewFileSink(Config{Dir: t.TempDir(), RotateInterval: 10 * time.Minute}, nil, "", nil)
	if err != nil {
		t.Fatalf("10m should be accepted: %v", err)
	}
	_ = s.Close()
}

// helpers

func newDirs(t *testing.T) dirs {
	t.Helper()
	root := t.TempDir()
	d := dirs{
		active:     filepath.Join(root, "active"),
		pending:    filepath.Join(root, "pending"),
		compressed: filepath.Join(root, "compressed"),
		uploaded:   filepath.Join(root, "uploaded"),
	}
	for _, p := range []string{d.active, d.pending, d.compressed, d.uploaded} {
		mustMkdir(t, p)
	}
	return d
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
