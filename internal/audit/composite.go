package audit

import "context"

// Recorders is a slice that itself satisfies Recorder, fanning each
// Record out to every contained Recorder. The slice-with-method shape
// (cf. http.HandlerFunc, sort.IntSlice) avoids a wrapper type.
type Recorders []Recorder

func (rs Recorders) Record(ctx context.Context, r *Record) {
	for _, rec := range rs {
		if rec == nil {
			continue
		}
		rec.Record(ctx, r)
	}
}

// Close closes every contained Recorder, returning the first error seen
// while still attempting the rest.
func (rs Recorders) Close() error {
	var firstErr error
	for _, rec := range rs {
		if rec == nil {
			continue
		}
		if err := rec.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
