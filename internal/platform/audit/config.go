package audit

import (
	"errors"
	"fmt"
	"time"
)

// Compression codecs for sealed files before upload.
const (
	CompressionGzip = "gzip"
	CompressionNone = "none"
)

// Config drives the local-rotate → compress → best-effort-upload → reap
// pipeline. Zero Dir disables the sink. S3 is optional: with an
// ObjectStore the shipper uploads then retains locally for Retention;
// without one the sink is a pure local rolling log reaped by age.
type Config struct {
	// Dir is the working root. The sink manages four subdirs under it:
	//   active/     the file currently being appended to
	//   pending/    sealed files awaiting compression
	//   compressed/ compressed files awaiting upload
	//   uploaded/   files already in object storage, kept until Retention
	Dir string

	// RotateInterval is the PRIMARY rotation trigger and the log-bucket
	// window: the active file is sealed at each clock boundary aligned to
	// this interval (e.g. 1h → one file per hour, matching the dt/hour
	// object partition; 10m → six per hour). Time-aligned buckets make a
	// file's coverage obvious at a glance when analyzing later.
	//
	// RotateMaxBytes is only a SAFETY cap: it seals early if a single
	// bucket balloons past this size so no object is unbounded. Under
	// normal load time is the trigger, not size (0 disables the cap).
	RotateInterval time.Duration
	RotateMaxBytes int64

	// UploadInterval is the maintenance cadence: each tick the shipper
	// compresses pending files, uploads compressed ones, then reaps.
	// Keeping it short bounds the plaintext-on-disk window and enforces
	// the disk cap frequently.
	UploadInterval time.Duration
	// Retention is how long an uploaded file is kept locally before the
	// reaper deletes it (applies to pending in local-only mode).
	Retention time.Duration
	// DiskCap bounds the on-disk footprint (active+pending+compressed+
	// uploaded). On breach the reaper drops oldest-uploaded first, then
	// oldest-compressed, then oldest-pending. 0 disables the cap.
	DiskCap int64

	// Compression codec applied when moving pending → compressed.
	// CompressionGzip (default) or CompressionNone. Audit JSONL is highly
	// compressible, so this multiplies both the disk and upload ceilings.
	Compression string
	// UploadConcurrency caps parallel uploads within one maintenance pass.
	UploadConcurrency int
}

func (c Config) withDefaults() Config {
	if c.RotateInterval <= 0 {
		c.RotateInterval = 10 * time.Minute // clock-aligned 10-minute buckets
	}
	if c.RotateMaxBytes == 0 {
		c.RotateMaxBytes = 128 << 20 // 128 MiB
	}
	if c.UploadInterval <= 0 {
		c.UploadInterval = 30 * time.Second
	}
	if c.Retention <= 0 {
		c.Retention = 7 * 24 * time.Hour
	}
	if c.DiskCap == 0 {
		c.DiskCap = 5 << 30 // 5 GiB
	}
	if c.Compression == "" {
		c.Compression = CompressionGzip
	}
	if c.UploadConcurrency <= 0 {
		c.UploadConcurrency = 4
	}
	return c
}

func (c Config) validate() error {
	if c.Dir == "" {
		return errors.New("audit: Dir is required")
	}
	// A cap below one rotation's worth is unenforceable: a single active
	// file can exceed it and the reaper never touches active/, so the cap
	// would stay breached. Fail fast rather than run permanently over.
	if c.DiskCap > 0 && c.RotateMaxBytes > 0 && c.DiskCap < c.RotateMaxBytes {
		return fmt.Errorf("audit: DiskCap (%d) must be >= RotateMaxBytes (%d)", c.DiskCap, c.RotateMaxBytes)
	}
	switch c.Compression {
	case CompressionGzip, CompressionNone:
	default:
		return fmt.Errorf("audit: Compression %q must be %q or %q", c.Compression, CompressionGzip, CompressionNone)
	}
	// Buckets are UTC clock-aligned by truncating to RotateInterval, which
	// anchors at the UTC epoch (a day boundary). Only an interval that
	// divides 24h evenly aligns to clean daily/hourly boundaries; an odd
	// interval (e.g. 7m) would drift across hours. Enforce it so the
	// dt/hour partition and the file's window always agree.
	if c.RotateInterval > 0 && 24*time.Hour%c.RotateInterval != 0 {
		return fmt.Errorf("audit: RotateInterval (%s) must divide 24h evenly for clock-aligned buckets (e.g. 1m, 5m, 10m, 15m, 30m, 1h, 2h, 6h, 12h)", c.RotateInterval)
	}
	return nil
}
