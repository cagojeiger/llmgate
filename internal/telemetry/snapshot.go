package telemetry

import (
	"encoding/json"

	"llmgate/internal/llmtypes"
)

// SnapshotEvent returns an immutable-enough copy for asynchronous delivery.
// Known event payloads are value-copied, including slices and Usage pointers;
// unknown Event implementations are returned as-is because telemetry cannot
// infer their copy semantics.
func SnapshotEvent(event Event) Event {
	switch rec := event.(type) {
	case *AuditEvent:
		if rec == nil {
			return (*AuditEvent)(nil)
		}
		cp := *rec
		return &cp
	case *CallEvent:
		return snapshotCall(rec)
	case *LLMCallFinalizedEvent:
		return snapshotLLMCallFinalized(rec)
	default:
		return event
	}
}

func snapshotCall(rec *CallEvent) *CallEvent {
	if rec == nil {
		return nil
	}
	cp := *rec
	cp.Usage = cloneUsage(rec.Usage)
	if len(rec.Attempts) > 0 {
		cp.Attempts = make([]llmtypes.Attempt, len(rec.Attempts))
		copy(cp.Attempts, rec.Attempts)
		for i := range cp.Attempts {
			cp.Attempts[i].Usage = cloneUsage(rec.Attempts[i].Usage)
		}
	}
	return &cp
}

func cloneUsage(usage *llmtypes.Usage) *llmtypes.Usage {
	if usage == nil {
		return nil
	}
	cp := *usage
	return &cp
}

func snapshotLLMCallFinalized(rec *LLMCallFinalizedEvent) *LLMCallFinalizedEvent {
	if rec == nil {
		return nil
	}
	cp := *rec
	cp.Request.RawJSON = cloneRawMessage(rec.Request.RawJSON)
	cp.Response.RawJSON = cloneRawMessage(rec.Response.RawJSON)
	cp.Usage = cloneUsage(rec.Usage)
	if rec.Error != nil {
		errCopy := *rec.Error
		cp.Error = &errCopy
	}
	if len(rec.Routing.Attempts) > 0 {
		cp.Routing.Attempts = make([]EventAttempt, len(rec.Routing.Attempts))
		copy(cp.Routing.Attempts, rec.Routing.Attempts)
		for i := range cp.Routing.Attempts {
			cp.Routing.Attempts[i].Usage = cloneUsage(rec.Routing.Attempts[i].Usage)
		}
	}
	if len(rec.Stream.Events) > 0 {
		cp.Stream.Events = make([]json.RawMessage, len(rec.Stream.Events))
		for i := range rec.Stream.Events {
			cp.Stream.Events[i] = cloneRawMessage(rec.Stream.Events[i])
		}
	}
	return &cp
}

func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}
