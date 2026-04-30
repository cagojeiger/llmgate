package audit

import "context"

// Composite fans out each Record to every contained Recorder. Used to
// run multiple back-ends in parallel (e.g. slog + Postgres + Prometheus).
type Composite []Recorder

func (c Composite) Record(ctx context.Context, r *Record) {
	for _, rec := range c {
		if rec == nil {
			continue
		}
		rec.Record(ctx, r)
	}
}

// Close closes every contained Recorder, returning the first error seen
// while still attempting the rest.
func (c Composite) Close() error {
	var firstErr error
	for _, rec := range c {
		if rec == nil {
			continue
		}
		if err := rec.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
