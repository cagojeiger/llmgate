package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type sonioxTestStoreFunc func(context.Context, string, string) error

func (f sonioxTestStoreFunc) Put(ctx context.Context, key, file string) error {
	return f(ctx, key, file)
}

func TestSonioxArchiveRoundTrip(t *testing.T) {
	store := newFakeStore()
	a, err := NewSonioxArchive(Config{Dir: t.TempDir()}, store, "audit", slogDiscard())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	c, err := a.Begin("test-request", "stt-rt-v6")
	if err != nil {
		t.Fatal(err)
	}
	want := []sonioxFrame{
		{Direction: "request", Kind: "text", Payload: []byte("{ \"type\": \"finalize\" }\n")},
		{Direction: "request", Kind: "binary", Payload: []byte{0, 0xff, 0x80, 0x42}},
		{Direction: "request", Kind: "binary", Payload: []byte{}},
		{Direction: "response", Kind: "text", Payload: []byte(`{"tokens":[{"text":"음성"}],"finished":true}`)},
	}
	for _, frame := range want {
		if err := c.Append(frame.Direction, frame.Kind, frame.Payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Finish(true, "finished"); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	key, raw := sonioxStoredFile(t, store)
	if !strings.HasPrefix(key, "audit/soniox-realtime/dt=") || !strings.HasSuffix(key, "-"+c.ID()+".jsonl.zst") {
		t.Fatalf("unexpected capture key: %q", key)
	}
	header, frames, footer := sonioxReadFile(t, raw)
	if header.CaptureID != c.ID() || header.RequestID != "test-request" || header.Model != "stt-rt-v6" {
		t.Fatalf("unexpected header: %+v", header)
	}
	if len(frames) != len(want) {
		t.Fatalf("got %d frames, want %d", len(frames), len(want))
	}
	for i, frame := range frames {
		if frame.Sequence != uint64(i+1) || frame.Direction != want[i].Direction || frame.Kind != want[i].Kind ||
			frame.Payload == nil || !bytes.Equal(frame.Payload, want[i].Payload) {
			t.Errorf("unexpected frame %d: %+v", i, frame)
		}
		if frame.OffsetNS < 0 || (i > 0 && frame.OffsetNS < frames[i-1].OffsetNS) {
			t.Errorf("frame %d offset out of order: %d", i, frame.OffsetNS)
		}
	}
	if !footer.Complete || footer.Reason != "finished" || footer.Frames != uint64(len(want)) ||
		footer.RequestBytes != int64(len(want[0].Payload)+len(want[1].Payload)) || footer.ResponseBytes != int64(len(want[3].Payload)) {
		t.Fatalf("unexpected footer: %+v", footer)
	}
}

func TestSonioxArchiveAwaitsConfirmedUpload(t *testing.T) {
	store := newFakeStore()
	started, release := make(chan struct{}, 1), make(chan struct{})
	gate := sonioxTestStoreFunc(func(ctx context.Context, key, file string) error {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return store.Put(ctx, key, file)
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	a, err := NewSonioxArchive(Config{Dir: t.TempDir(), UploadInterval: time.Hour}, gate, "audit", logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	c := newSonioxTestCapture(t, a)
	done := finishSonioxTestCapture(c)
	waitSonioxTestSignal(t, started)
	select {
	case err := <-done:
		t.Fatalf("Finish returned before object storage acknowledged the upload: %v", err)
	default:
	}
	close(release)
	if err := waitSonioxTestFinish(t, done); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	key, raw := sonioxStoredFile(t, store)
	_, _, footer := sonioxReadFile(t, raw)
	if !footer.Complete {
		t.Fatal("uploaded capture is incomplete")
	}
	for _, want := range []string{`"msg":"soniox archive upload confirmed"`, `"request_id":"test-request"`,
		`"capture_id":"` + c.ID() + `"`, `"object_key":"` + key + `"`} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("missing upload confirmation metadata %q in %s", want, logs.String())
		}
	}
}

func TestSonioxArchiveKeepsCompleteLocalCaptureAfterUploadFailure(t *testing.T) {
	store := newFakeStore()
	var attempts atomic.Int32
	failure := errors.New("synthetic upload failure")
	flaky := sonioxTestStoreFunc(func(ctx context.Context, key, file string) error {
		if attempts.Add(1) == 1 {
			return failure
		}
		return store.Put(ctx, key, file)
	})
	cfg := Config{Dir: t.TempDir(), Compression: CompressionNone, UploadInterval: time.Hour}
	a, err := NewSonioxArchive(cfg, flaky, "", slogDiscard())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	c := newSonioxTestCapture(t, a)
	if err := c.Finish(true, "finished"); !errors.Is(err, failure) {
		t.Fatalf("Finish = %v, want upload error", err)
	}
	local, err := os.ReadFile(filepath.Join(a.dirs.compressed, c.name))
	if err != nil {
		t.Fatal(err)
	}
	_, _, footer := sonioxReadFile(t, local)
	if !footer.Complete || store.count() != 0 {
		t.Fatalf("local capture should be complete and upload pending: %+v", footer)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	_, raw := sonioxStoredFile(t, store)
	if attempts.Load() != 2 || !bytes.Equal(raw, local) {
		t.Fatal("shipping retry did not preserve the exact sealed capture")
	}
}

func TestSonioxArchiveWaitTimeoutDoesNotCancelUpload(t *testing.T) {
	store := newFakeStore()
	started, release, put := make(chan struct{}, 1), make(chan struct{}), make(chan struct{}, 1)
	gate := sonioxTestStoreFunc(func(ctx context.Context, key, file string) error {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
			if err := store.Put(ctx, key, file); err != nil {
				return err
			}
			put <- struct{}{}
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	a, err := NewSonioxArchive(Config{Dir: t.TempDir(), UploadInterval: time.Hour}, gate, "", slogDiscard())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	a.uploadWait = 100 * time.Millisecond
	done := finishSonioxTestCapture(newSonioxTestCapture(t, a))
	waitSonioxTestSignal(t, started)
	if err := waitSonioxTestFinish(t, done); err == nil || !strings.Contains(err.Error(), "upload confirmation timed out") {
		t.Fatalf("Finish = %v, want upload confirmation timeout", err)
	}
	close(release)
	waitSonioxTestSignal(t, put)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	_, raw := sonioxStoredFile(t, store)
	_, _, footer := sonioxReadFile(t, raw)
	if !footer.Complete {
		t.Fatal("late upload should retain the complete capture")
	}
}

func TestSonioxArchiveCloseUnblocksUploadWait(t *testing.T) {
	store := newFakeStore()
	started := make(chan struct{}, 1)
	var attempts atomic.Int32
	gate := sonioxTestStoreFunc(func(ctx context.Context, key, file string) error {
		if attempts.Add(1) == 1 {
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}
		return store.Put(ctx, key, file)
	})
	a, err := NewSonioxArchive(Config{Dir: t.TempDir(), UploadInterval: time.Hour}, gate, "", slogDiscard())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	done := finishSonioxTestCapture(newSonioxTestCapture(t, a))
	waitSonioxTestSignal(t, started)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := waitSonioxTestFinish(t, done); err == nil {
		t.Fatal("canceled upload incorrectly acknowledged completion")
	}
	if attempts.Load() != 2 || store.count() != 1 {
		t.Fatal("shutdown flush did not retry the retained capture")
	}
}

func newSonioxTestCapture(t *testing.T, a *SonioxArchive) *SonioxCapture {
	t.Helper()
	c, err := a.Begin("test-request", "stt-rt-v6")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Append("request", "binary", []byte{0, 0xff}); err != nil {
		t.Fatal(err)
	}
	return c
}

func finishSonioxTestCapture(c *SonioxCapture) <-chan error {
	done := make(chan error, 1)
	go func() { done <- c.Finish(true, "finished") }()
	return done
}

func waitSonioxTestSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("object storage operation did not start promptly")
	}
}

func waitSonioxTestFinish(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("capture did not finish promptly")
		return nil
	}
}

func TestSonioxArchiveReservesSpaceAndSealsLimitFailureIncomplete(t *testing.T) {
	store := newFakeStore()
	a, err := NewSonioxArchive(Config{Dir: t.TempDir(), Compression: CompressionNone, DiskCap: 2048}, store, "", slogDiscard())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	c, err := a.Begin("first", "stt-rt-v6")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Begin("concurrent", "stt-rt-v6"); err == nil {
		t.Fatal("opened another capture while first reservation consumed the cap")
	}
	if err := c.Append("request", "binary", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Append("request", "binary", make([]byte, 2048)); err == nil {
		t.Fatal("accepted frame that exceeded the capture budget")
	}
	if err := c.Finish(true, "finished"); err == nil {
		t.Fatal("reported a complete capture after failing to record a frame")
	}
	if _, err := a.Begin("after-limit", "stt-rt-v6"); err == nil {
		t.Fatal("admitted capture by replacing an unuploaded capture")
	}
	if len(listFiles(a.dirs.pending)) != 1 {
		t.Fatal("unuploaded capture was not kept")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	_, raw := sonioxStoredFile(t, store)
	_, frames, footer := sonioxReadFile(t, raw)
	if len(frames) != 1 || frames[0].Payload == nil || len(frames[0].Payload) != 0 || footer.Complete || footer.Reason != "capture_write_failed" {
		t.Fatalf("unexpected partial capture: frames=%+v footer=%+v", frames, footer)
	}
}

func TestSonioxArchiveRecoversInterruptedCapture(t *testing.T) {
	for _, tc := range []struct {
		name      string
		writeTail func(*SonioxCapture) error
	}{
		{"torn frame", func(c *SonioxCapture) error {
			_, err := c.file.WriteString(`{"type":"frame","sequence":2,"payload":"unfin`)
			return err
		}},
		{"unpublished complete footer", func(c *SonioxCapture) error {
			footer := c.footer
			footer.Type, footer.Complete, footer.Reason = "footer", true, "finished"
			footer.SHA256 = hex.EncodeToString(c.digest.Sum(nil))
			return writeSonioxFooter(c.file, c.size, footer)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Dir: t.TempDir(), Compression: CompressionNone}
			a, err := NewSonioxArchive(cfg, nil, "", slogDiscard())
			if err != nil {
				t.Fatal(err)
			}
			// Stop maintenance, then leave the active file in the state a
			// process crash would leave it. Close would otherwise seal it.
			a.cancel()
			<-a.done
			c, err := a.Begin("interrupted", "stt-rt-v6")
			if err != nil {
				t.Fatal(err)
			}
			payload := []byte(`{"tokens":[{"text":"partial"}]}`)
			if err := c.Append("response", "text", payload); err != nil {
				t.Fatal(err)
			}
			if err := tc.writeTail(c); err != nil {
				t.Fatal(err)
			}
			if err := c.file.Close(); err != nil {
				t.Fatal(err)
			}
			store := newFakeStore()
			recovered, err := NewSonioxArchive(cfg, store, "", slogDiscard())
			if err != nil {
				t.Fatal(err)
			}
			if err := recovered.Close(); err != nil {
				t.Fatal(err)
			}
			_, raw := sonioxStoredFile(t, store)
			h, frames, footer := sonioxReadFile(t, raw)
			if h.CaptureID != c.ID() || len(frames) != 1 || !bytes.Equal(frames[0].Payload, payload) ||
				footer.Complete || footer.Reason != "process_restarted" || footer.Frames != 1 || footer.ResponseBytes != int64(len(payload)) {
				t.Fatalf("unexpected recovered archive: header=%+v frames=%+v footer=%+v", h, frames, footer)
			}
		})
	}
}

func TestSonioxArchiveCloseSealsActiveCapture(t *testing.T) {
	dir := t.TempDir()
	a, err := NewSonioxArchive(Config{Dir: dir, Compression: CompressionNone}, nil, "", slogDiscard())
	if err != nil {
		t.Fatal(err)
	}
	c, err := a.Begin("test-request", "stt-rt-v6")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Append("request", "binary", []byte{0, 0}); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Finish(true, "finished"); err == nil {
		t.Fatal("reported completion after archive shutdown sealed capture incomplete")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "soniox-realtime", "compressed", c.name))
	if err != nil {
		t.Fatal(err)
	}
	_, _, footer := sonioxReadFile(t, raw)
	if footer.Complete || footer.Reason != "server_shutdown" || footer.Frames != 1 {
		t.Fatalf("unexpected shutdown footer: %+v", footer)
	}
}

func sonioxStoredFile(t *testing.T, store *fakeStore) (string, []byte) {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.puts) != 1 {
		t.Fatalf("got %d stored objects, want one", len(store.puts))
	}
	for key, data := range store.puts {
		return key, data
	}
	return "", nil
}

func sonioxReadFile(t *testing.T, raw []byte) (sonioxHeader, []sonioxFrame, sonioxFooter) {
	t.Helper()
	lines := bytes.SplitAfter(raw, []byte("\n"))
	if len(lines) < 3 || len(lines[len(lines)-1]) != 0 {
		t.Fatalf("unterminated or invalid capture: %q", raw)
	}
	var header sonioxHeader
	if err := json.Unmarshal(lines[0], &header); err != nil || header.Type != "header" || header.Version != 1 {
		t.Fatalf("invalid capture header: %v %+v", err, header)
	}
	var footer sonioxFooter
	if err := json.Unmarshal(lines[len(lines)-2], &footer); err != nil || footer.Type != "footer" {
		t.Fatalf("invalid capture footer: %v %+v", err, footer)
	}
	digest := sha256.Sum256(bytes.Join(lines[:len(lines)-2], nil))
	if got := hex.EncodeToString(digest[:]); footer.SHA256 != got {
		t.Fatalf("digest = %q, want %q", footer.SHA256, got)
	}
	frames := make([]sonioxFrame, 0, len(lines)-3)
	for _, line := range lines[1 : len(lines)-2] {
		var frame sonioxFrame
		if err := json.Unmarshal(line, &frame); err != nil || frame.Type != "frame" {
			t.Fatalf("invalid capture frame: %v %+v", err, frame)
		}
		frames = append(frames, frame)
	}
	return header, frames, footer
}
