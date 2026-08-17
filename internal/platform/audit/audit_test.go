package audit

import (
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

	// First pass: seal + a failing upload leaves the file in pending.
	s.mu.Lock()
	s.rotateLocked()
	s.mu.Unlock()
	s.shipper.pass(ctx)
	if got := len(listFiles(s.dirs.pending)); got != 1 {
		t.Fatalf("pending after failed upload = %d, want 1", got)
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
	// Local-only: sealed file stays in pending (no store to move it).
	if got := len(listFiles(s.dirs.pending)); got != 1 {
		t.Fatalf("pending files = %d, want 1 in local-only mode", got)
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
	sh.reapPass()

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
	sh.enforceDiskCap()

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

// helpers

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
