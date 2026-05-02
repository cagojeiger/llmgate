package provider

import "time"

// Attempt records the outcome of one upstream call inside a single
// gateway request. A non-fallback request produces exactly one Attempt;
// a fallback chain that retries N times produces N — typically N-1 with
// errors followed by one success.
//
// Usage may be nil when the upstream rejected before generation (4xx,
// pre-stream 5xx). For mid-stream truncation, adapters surface partial
// usage via Stream.Summary so the value here can be non-nil even with
// a non-success ErrorKind.
type Attempt struct {
	Vendor     string
	Model      string
	StartedAt  time.Time
	DurationMS int64
	StatusCode int
	ErrorKind  Kind
	Usage      *Usage
	VendorCost string
}
