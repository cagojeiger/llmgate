package provider

import "time"

// Attempt records one upstream call inside a gateway request.
// Fallback chains append one Attempt per provider actually called.
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
