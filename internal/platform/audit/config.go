package audit

import (
	"errors"
	"fmt"
	"time"
)

// Config drives the local-rotate + best-effort-upload audit sink. Zero
// Dir disables the sink entirely (buildResultSink falls through). S3 is
// optional: with an ObjectStore the shipper uploads then retains locally
// for Retention; without one the sink is a pure local rolling log reaped
// by age.
type Config struct {
	// Dir is the working root. The sink manages three subdirs under it:
	//   active/   the file currently being appended to
	//   pending/  sealed files awaiting upload
	//   uploaded/ files already in object storage, kept until Retention
	Dir string

	// RotateInterval seals the active file on a wall-clock cadence even
	// when it stays small. Time-based rotation bounds the crash-loss
	// window and maps cleanly onto the dt/hour object partitions.
	RotateInterval time.Duration
	// RotateMaxBytes seals early once the active file reaches this size,
	// keeping any single object bounded. 0 disables the size trigger.
	RotateMaxBytes int64

	// UploadInterval is how often the shipper scans pending/ and pushes
	// to the store. Ignored when Store is nil.
	UploadInterval time.Duration
	// Retention is how long an uploaded file is kept locally before the
	// reaper deletes it. In local-only mode (Store nil) it applies to
	// pending/ files by seal age instead.
	Retention time.Duration
	// DiskCap bounds the on-disk footprint (active+pending+uploaded). On
	// breach the reaper drops oldest-uploaded first, then oldest-pending
	// — accepted because this is best-effort, loss-tolerant data. 0
	// disables the cap.
	DiskCap int64
}

// withDefaults fills unset knobs. Kept separate from validate so tests
// can construct a minimal Config and still get sane cadences.
func (c Config) withDefaults() Config {
	if c.RotateInterval <= 0 {
		c.RotateInterval = time.Hour
	}
	if c.RotateMaxBytes == 0 {
		c.RotateMaxBytes = 128 << 20 // 128 MiB
	}
	if c.UploadInterval <= 0 {
		c.UploadInterval = time.Hour
	}
	if c.Retention <= 0 {
		c.Retention = 7 * 24 * time.Hour
	}
	if c.DiskCap == 0 {
		c.DiskCap = 5 << 30 // 5 GiB
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
	return nil
}
