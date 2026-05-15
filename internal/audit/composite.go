package audit

import "context"

// Recorders fans each operational Record out to every contained Recorder.
type Recorders []Recorder

func (rs Recorders) RecordAudit(ctx context.Context, r *Record) {
	for _, rec := range rs {
		if rec == nil {
			continue
		}
		rec.RecordAudit(ctx, r)
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

// CallRecorders fans each CallRecord out to every contained CallRecorder.
type CallRecorders []CallRecorder

func (rs CallRecorders) RecordCall(ctx context.Context, r *CallRecord) {
	for _, rec := range rs {
		if rec == nil {
			continue
		}
		rec.RecordCall(ctx, r)
	}
}

func (rs CallRecorders) Close() error {
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
