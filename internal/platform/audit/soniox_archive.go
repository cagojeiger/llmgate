package audit

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"
)

const (
	sonioxArchiveRoot = "soniox-realtime"
	sonioxSessionMax  = 256 << 20 // encoded bytes, including the footer
	sonioxFrameMax    = 16 << 20  // same as the realtime WebSocket read limit
	sonioxFooterRoom  = 512
	sonioxCodecRoom   = 1 << 20 // in addition to the largest possible input to the serial compressor
	sonioxUploadWait  = 30 * time.Second
)

// SonioxArchive stores each session in a separate file so the normal audit
// stream continues to contain one finalized result per request. Admissions
// reserve a full session's budget; an outage may deny new sessions rather
// than evict a capture that has not reached object storage.
type SonioxArchive struct {
	dirs         dirs
	shipper      *shipper
	log          *slog.Logger
	interval     time.Duration
	limit        int64
	admissionCap int64
	compression  string
	uploadWait   time.Duration
	wake         chan struct{}

	mu      sync.Mutex
	closed  bool
	active  map[string]*SonioxCapture
	uploads map[string]chan sonioxUploadResult // keyed by the file the ObjectStore receives

	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

type sonioxHeader struct {
	Type      string    `json:"type"`
	Version   int       `json:"version"`
	CaptureID string    `json:"capture_id"`
	RequestID string    `json:"request_id"`
	Model     string    `json:"model"`
	StartedAt time.Time `json:"started_at"`
}

type sonioxFrame struct {
	Type      string `json:"type"`
	Sequence  uint64 `json:"sequence"`
	OffsetNS  int64  `json:"offset_ns"`
	Direction string `json:"direction"`
	Kind      string `json:"kind"`
	Payload   []byte `json:"payload"` // JSON encodes bytes as base64, including an empty frame as "".
}

type sonioxFooter struct {
	Type          string `json:"type"`
	Complete      bool   `json:"complete"`
	Reason        string `json:"reason"`
	Frames        uint64 `json:"frames"`
	RequestBytes  int64  `json:"request_bytes"`
	ResponseBytes int64  `json:"response_bytes"`
	SHA256        string `json:"sha256"` // exact header and frame lines, with their terminating newlines
}

// SonioxCapture writes frames as they arrive without keeping the session in
// memory. Append and Finish may be called from different relay goroutines.
type SonioxCapture struct {
	archive   *SonioxArchive
	id        string
	name      string
	requestID string
	started   time.Time

	mu        sync.Mutex
	file      *os.File
	digest    hash.Hash
	size      int64
	footer    sonioxFooter
	appendErr error
	finishErr error
}

// NewSonioxArchive reuses the ordinary audit retention and object store while
// separating both local capture files and remote objects from result JSONL.
func NewSonioxArchive(cfg Config, store ObjectStore, prefix string, log *slog.Logger) (*SonioxArchive, error) {
	cfg = cfg.withDefaults()
	cfg.RotateMaxBytes = 0 // a capture is sealed per session, not on generic audit rotation
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	limit, admissionCap := min(int64(sonioxSessionMax), cfg.DiskCap), cfg.DiskCap
	if cfg.Compression != CompressionNone {
		limit = min(limit, (cfg.DiskCap-sonioxCodecRoom)/2)
		admissionCap -= limit + sonioxCodecRoom
	}
	if limit < 2*sonioxFooterRoom {
		return nil, errors.New("soniox archive: disk cap cannot accommodate a capture")
	}
	if log == nil {
		log = slog.Default()
	}
	root := filepath.Join(cfg.Dir, sonioxArchiveRoot)
	d := dirs{
		active: filepath.Join(root, "active"), pending: filepath.Join(root, "pending"),
		compressed: filepath.Join(root, "compressed"), uploaded: filepath.Join(root, "uploaded"),
	}
	for _, dir := range []string{d.active, d.pending, d.compressed, d.uploaded} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("soniox archive: mkdir: %w", err)
		}
	}
	cfg.DiskCap = 0 // retention still applies, but only admission enforces a cap
	log = log.With("archive", sonioxArchiveRoot)
	a := &SonioxArchive{
		dirs: d, log: log, interval: cfg.RotateInterval, limit: limit, admissionCap: admissionCap,
		compression: cfg.Compression, uploadWait: sonioxUploadWait, wake: make(chan struct{}, 1),
		active: make(map[string]*SonioxCapture), done: make(chan struct{}),
	}
	if store != nil {
		a.uploads = make(map[string]chan sonioxUploadResult)
		store = sonioxNotifyingStore{store: store, notify: a.notifyUpload}
	}
	a.shipper = newShipper(d, store, path.Join(prefix, sonioxArchiveRoot), cfg, log)
	if err := a.recoverOrphans(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	go a.run(ctx, cfg.UploadInterval)
	return a, nil
}

func (a *SonioxArchive) run(ctx context.Context, interval time.Duration) {
	defer close(a.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-a.wake:
		}
		if ctx.Err() == nil {
			a.shipper.pass(ctx)
		}
	}
}

func (a *SonioxArchive) Begin(requestID, model string) (*SonioxCapture, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, errors.New("soniox archive: closed")
	}
	if err := a.checkAdmission(); err != nil {
		return nil, err
	}
	started, id := time.Now(), rand.Text()
	name := fmt.Sprintf("soniox-%s-%s.jsonl", bucketStartOf(started, a.interval).Format("20060102T150405Z"), id)
	header := sonioxHeader{Type: "header", Version: 1, CaptureID: id, RequestID: requestID, Model: model, StartedAt: started.UTC()}
	line, err := sonioxLine(header)
	if err != nil {
		return nil, err
	}
	if int64(len(line))+sonioxFooterRoom > a.limit {
		return nil, errors.New("soniox archive: header exceeds capture budget")
	}
	f, err := os.OpenFile(filepath.Join(a.dirs.active, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("soniox archive: create capture: %w", err)
	}
	c := &SonioxCapture{archive: a, id: id, name: name, requestID: requestID, started: started, file: f, digest: sha256.New()}
	if err := c.writeLine(line); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, err
	}
	a.active[name] = c
	return c, nil
}

// Scan stages from oldest in the pipeline to newest. A concurrent forward
// rename can count a file twice but cannot make it disappear from the sum.
// Begin/Finish share the lock for the active reservation and its handoff.
func (a *SonioxArchive) checkAdmission() error {
	remaining := a.admissionCap - (int64(len(a.active))+1)*a.limit
	for _, dir := range []string{a.dirs.active, a.dirs.pending, a.dirs.compressed, a.dirs.uploaded} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("soniox archive: count staged bytes: %w", err)
		}
		for _, entry := range entries {
			if entry.IsDir() || (dir == a.dirs.active && a.active[entry.Name()] != nil) {
				continue
			}
			info, err := entry.Info()
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return fmt.Errorf("soniox archive: stat staged file: %w", err)
			}
			remaining -= info.Size()
		}
	}
	if remaining < 0 {
		return errors.New("soniox archive: no space for another capture")
	}
	return nil
}

func (c *SonioxCapture) ID() string { return c.id }

func (c *SonioxCapture) Append(direction, kind string, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.file == nil {
		return errors.New("soniox archive: capture finished")
	}
	if c.appendErr != nil {
		return c.appendErr
	}
	c.appendErr = c.appendFrame(direction, kind, payload)
	return c.appendErr
}

func (c *SonioxCapture) appendFrame(direction, kind string, payload []byte) error {
	if !validSonioxFrame(direction, kind) {
		return errors.New("soniox archive: invalid frame direction or kind")
	}
	remaining := c.archive.limit - c.size - sonioxFooterRoom
	if len(payload) > sonioxFrameMax || int64(base64.StdEncoding.EncodedLen(len(payload))) > remaining {
		return errors.New("soniox archive: capture size limit reached")
	}
	if payload == nil {
		payload = []byte{}
	}
	frame := sonioxFrame{
		Type: "frame", Sequence: c.footer.Frames + 1, OffsetNS: time.Since(c.started).Nanoseconds(),
		Direction: direction, Kind: kind, Payload: payload,
	}
	line, err := sonioxLine(frame)
	if err != nil {
		return err
	}
	if int64(len(line)) > remaining {
		return errors.New("soniox archive: capture size limit reached")
	}
	if err := c.writeLine(line); err != nil {
		return err
	}
	c.footer.count(frame)
	return nil
}

func (c *SonioxCapture) writeLine(line []byte) error {
	if _, err := c.file.Write(line); err != nil {
		return fmt.Errorf("soniox archive: write capture: %w", err)
	}
	_, _ = c.digest.Write(line)
	c.size += int64(len(line))
	return nil
}

func (f *sonioxFooter) count(frame sonioxFrame) {
	f.Frames++
	if frame.Direction == "request" {
		f.RequestBytes += int64(len(frame.Payload))
	} else {
		f.ResponseBytes += int64(len(frame.Payload))
	}
}

func validSonioxFrame(direction, kind string) bool {
	return (direction == "request" || direction == "response") && (kind == "text" || kind == "binary")
}

// Finish seals the file after its terminal record and contents are synced. A
// complete capture also awaits the configured object store. A failed or
// unconfirmed upload leaves the sealed file available for background retries.
func (c *SonioxCapture) Finish(complete bool, reason string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.file == nil {
		if complete && !c.footer.Complete && c.finishErr == nil {
			return errors.New("soniox archive: capture sealed incomplete")
		}
		return c.finishErr
	}
	if c.appendErr != nil {
		complete, reason = false, "capture_write_failed"
	}
	c.footer.Type, c.footer.Complete = "footer", complete
	c.footer.Reason = sonioxReason(complete, reason)
	c.footer.SHA256 = hex.EncodeToString(c.digest.Sum(nil))
	sealErr := writeSonioxFooter(c.file, c.size, c.footer)
	closeErr := c.file.Close()
	c.file = nil
	object, publishErr := c.archive.release(c.name, sealErr == nil && closeErr == nil, complete)
	c.finishErr = errors.Join(c.appendErr, sealErr, closeErr, publishErr)
	if object != "" {
		c.archive.log.Info("soniox archive upload confirmed", "request_id", c.requestID, "capture_id", c.id, "object_key", object)
	}
	return c.finishErr
}

type sonioxUploadResult struct {
	key string
	err error
}

// Notify from Put itself: an uploaded file is durable even if moving its local
// copy into uploaded/ fails. Only this archive's ObjectStore is wrapped.
type sonioxNotifyingStore struct {
	store  ObjectStore
	notify func(name string, result sonioxUploadResult)
}

func (s sonioxNotifyingStore) Put(ctx context.Context, key, file string) error {
	err := s.store.Put(ctx, key, file)
	s.notify(filepath.Base(file), sonioxUploadResult{key: key, err: err})
	return err
}

func (a *SonioxArchive) notifyUpload(name string, result sonioxUploadResult) {
	a.mu.Lock()
	waiter := a.uploads[name]
	delete(a.uploads, name)
	a.mu.Unlock()
	if waiter != nil {
		waiter <- result
	}
}

func (a *SonioxArchive) release(name string, sealed, complete bool) (string, error) {
	a.mu.Lock()
	delete(a.active, name)
	if !sealed {
		a.mu.Unlock()
		return "", nil
	}
	var waiter chan sonioxUploadResult
	objectName := name + compressedExt(a.compression)
	if complete && a.uploads != nil {
		// Register before making the sealed file visible to the shipper.
		waiter = make(chan sonioxUploadResult, 1)
		a.uploads[objectName] = waiter
	}
	err := a.publish(name)
	a.mu.Unlock()
	if waiter == nil {
		return "", err
	}
	defer func() {
		a.mu.Lock()
		delete(a.uploads, objectName)
		a.mu.Unlock()
	}()
	if err != nil {
		return "", err
	}
	select {
	case a.wake <- struct{}{}:
	default:
	}

	timer := time.NewTimer(a.uploadWait)
	defer timer.Stop()
	select {
	case result := <-waiter:
		if result.err != nil {
			return "", fmt.Errorf("soniox archive: upload failed: %w", result.err)
		}
		return result.key, nil
	case <-timer.C:
		return "", errors.New("soniox archive: upload confirmation timed out")
	case <-a.done:
		return "", errors.New("soniox archive: closed before upload confirmation")
	}
}

// Reasons are labels, never messages from either WebSocket or a raw error.
func sonioxReason(complete bool, reason string) string {
	if complete {
		return "finished"
	}
	switch reason {
	case "capture_write_failed", "client_disconnected", "client_protocol_error", "client_message_too_large",
		"upstream_dial_failed", "upstream_write_failed", "upstream_disconnected", "upstream_error",
		"upstream_protocol_error", "upstream_message_too_large", "drain_timeout", "session_timeout", "server_shutdown":
		return reason
	default:
		return "interrupted"
	}
}

func writeSonioxFooter(f *os.File, offset int64, footer sonioxFooter) error {
	line, err := sonioxLine(footer)
	if err != nil {
		return err
	}
	if len(line) > sonioxFooterRoom {
		return errors.New("soniox archive: footer exceeds reserved space")
	}
	// Discard a torn frame or the stale footer of a file interrupted before
	// publication. Writing at the last committed offset keeps JSONL intact.
	if err := f.Truncate(offset); err != nil {
		return fmt.Errorf("soniox archive: truncate unsealed tail: %w", err)
	}
	if _, err := f.WriteAt(line, offset); err != nil {
		return fmt.Errorf("soniox archive: write footer: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("soniox archive: sync capture: %w", err)
	}
	return nil
}

func (a *SonioxArchive) publish(name string) error {
	if err := os.Rename(filepath.Join(a.dirs.active, name), filepath.Join(a.dirs.pending, name)); err != nil {
		return fmt.Errorf("soniox archive: publish sealed file: %w", err)
	}
	return nil
}

func (a *SonioxArchive) recoverOrphans() error {
	entries, err := os.ReadDir(a.dirs.active)
	if err != nil {
		return fmt.Errorf("soniox archive: list unsealed files: %w", err)
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		name := entry.Name()
		if err := recoverSonioxCapture(filepath.Join(a.dirs.active, name)); err != nil {
			if errors.Is(err, errSonioxMissingHeader) {
				// Keep a damaged header staged and counted against admissions.
				// Without a readable identity it cannot be published safely.
				a.log.Warn("soniox archive: kept unreadable unsealed capture", "file", name)
				continue
			}
			return fmt.Errorf("soniox archive: recover unsealed capture: %w", err)
		}
		if err := a.publish(name); err != nil {
			return err
		}
	}
	return nil
}

var errSonioxMissingHeader = errors.New("soniox archive: incomplete header")

func recoverSonioxCapture(filename string) (err error) {
	f, err := os.OpenFile(filename, os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 64<<10), base64.StdEncoding.EncodedLen(sonioxFrameMax)+sonioxFooterRoom)
	s.Split(sonioxCompleteLine)
	var header sonioxHeader
	if !s.Scan() || json.Unmarshal(s.Bytes(), &header) != nil || header.Type != "header" || header.Version != 1 || header.CaptureID == "" {
		if err := s.Err(); err != nil {
			return err
		}
		return errSonioxMissingHeader
	}
	digest := sha256.New()
	_, _ = digest.Write(s.Bytes())
	offset := int64(len(s.Bytes()))
	footer := sonioxFooter{Type: "footer", Reason: "process_restarted"}
	for s.Scan() {
		var frame sonioxFrame
		if json.Unmarshal(s.Bytes(), &frame) != nil || frame.Type != "frame" || frame.Sequence != footer.Frames+1 ||
			frame.Payload == nil || !validSonioxFrame(frame.Direction, frame.Kind) {
			break
		}
		footer.count(frame)
		_, _ = digest.Write(s.Bytes())
		offset += int64(len(s.Bytes()))
	}
	if err := s.Err(); err != nil {
		return err
	}
	footer.SHA256 = hex.EncodeToString(digest.Sum(nil))
	return writeSonioxFooter(f, offset, footer)
}

func sonioxCompleteLine(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i+1], nil
	}
	if atEOF {
		return 0, nil, bufio.ErrFinalToken // do not promote a torn final line
	}
	return 0, nil, nil
}

func sonioxLine(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func (a *SonioxArchive) Close() error {
	a.closeOnce.Do(func() {
		a.mu.Lock()
		a.closed = true
		active := make([]*SonioxCapture, 0, len(a.active))
		for _, c := range a.active {
			active = append(active, c)
		}
		a.mu.Unlock()
		a.cancel()
		<-a.done
		for _, c := range active {
			a.closeErr = errors.Join(a.closeErr, c.Finish(false, "server_shutdown"))
		}
		ctx, cancel := context.WithTimeout(context.Background(), finalFlushTimeout)
		defer cancel()
		a.shipper.flush(ctx)
	})
	return a.closeErr
}
