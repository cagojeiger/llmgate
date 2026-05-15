package telemetry

import "context"

// AuditRecorders fans each operational AuditEvent out to every contained AuditRecorder.
type AuditRecorders []AuditRecorder

func (rs AuditRecorders) RecordAudit(ctx context.Context, r *AuditEvent) {
	for _, rec := range rs {
		if rec == nil {
			continue
		}
		rec.RecordAudit(ctx, r)
	}
}

// Close closes every contained AuditRecorder, returning the first error seen
// while still attempting the rest.
func (rs AuditRecorders) Close() error {
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

// CallRecorders fans each CallEvent out to every contained CallRecorder.
type CallRecorders []CallRecorder

func (rs CallRecorders) RecordCall(ctx context.Context, r *CallEvent) {
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
