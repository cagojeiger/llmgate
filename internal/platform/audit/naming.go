package audit

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// instanceID identifies the writer that produced a sealed file so that
// object keys never collide across replicas. In Kubernetes the pod name
// (injected via the downward API as POD_NAME) is already unique per pod
// instance — it carries the ReplicaSet hash and a random suffix — so we
// prefer it. Outside k8s we fall back to the hostname, then to a random
// token so a missing hostname can never make two writers share a prefix.
func instanceID() string {
	if v := sanitize(os.Getenv("POD_NAME")); v != "" {
		return v
	}
	if h, err := os.Hostname(); err == nil {
		if v := sanitize(h); v != "" {
			return v
		}
	}
	return "host-" + randToken()
}

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// sanitize keeps a token safe as both a filename and an S3 key segment.
func sanitize(s string) string {
	s = unsafeName.ReplaceAllString(strings.TrimSpace(s), "-")
	return strings.Trim(s, "-.")
}

// randToken returns 8 hex chars from crypto/rand. It is the guaranteed-
// unique component of a sealed filename: even if two seals land in the
// same second on the same instance, the random suffix keeps their names
// (and therefore their object keys) distinct. crypto/rand keeps this a
// stdlib-only dependency.
func randToken() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read only fails if the OS entropy source is broken; fall
		// back to a nanosecond stamp so naming stays total rather than
		// panicking on the audit path.
		return fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff)
	}
	return hex.EncodeToString(b[:])
}

// sealedName is the filename a rotation assigns when it seals the active
// file. It is fixed at seal time and reused verbatim as the object key's
// leaf, which is what makes uploads idempotent: a retry after a crash
// between PutObject and local delete re-puts the same key.
func sealedName(instance string, sealedAt time.Time) string {
	return fmt.Sprintf("%s-%s-%s.jsonl", instance, sealedAt.UTC().Format("20060102T150405Z"), randToken())
}

var sealTimeRE = regexp.MustCompile(`(\d{8}T\d{6}Z)`)

// parseSealTime recovers the seal instant from a filename. Pod names
// contain hyphens, so we match the fixed timestamp token by pattern
// rather than splitting on "-". ok is false for names we did not write.
func parseSealTime(name string) (time.Time, bool) {
	m := sealTimeRE.FindString(name)
	if m == "" {
		return time.Time{}, false
	}
	t, err := time.Parse("20060102T150405Z", m)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// objectKey time-partitions sealed files so downstream scanners (DuckDB,
// Athena, ClickHouse) can prune by dt/hour without opening objects. The
// partition is derived from the seal time encoded in name is irrelevant;
// we pass the seal time explicitly to keep key and content consistent.
func objectKey(prefix string, sealedAt time.Time, name string) string {
	t := sealedAt.UTC()
	key := fmt.Sprintf("dt=%s/hour=%02d/%s", t.Format("2006-01-02"), t.Hour(), name)
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return key
	}
	return prefix + "/" + key
}
