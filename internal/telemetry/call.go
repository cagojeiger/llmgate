package telemetry

import (
	"time"

	"llmgate/internal/llmrouter"
	"llmgate/internal/llmtypes"
)

// CallEvent captures the result of one gateway LLM request. Attempts contains
// the vendor/model history for that request; a CallEvent is emitted only after
// at least one vendor attempt exists.
type CallEvent struct {
	EventCommon

	ModelRequested string
	ModelUsed      string
	Vendor         string

	RequestBytes  int64
	ResponseBytes int64

	Usage      *llmtypes.Usage
	VendorCost string

	Attempts []llmtypes.Attempt
}

func NewCallEvent(common EventCommon, modelRequested string, requestBytes int64) *CallEvent {
	return &CallEvent{
		EventCommon:    common,
		ModelRequested: modelRequested,
		RequestBytes:   requestBytes,
	}
}

func (*CallEvent) TelemetryEventType() string { return EventTypeCall }

func CallAttempted(c *CallEvent) bool {
	return c != nil && len(c.Attempts) > 0
}

func AttemptsCount(c *CallEvent) int {
	if c == nil {
		return 0
	}
	return len(c.Attempts)
}

func FinalAttempt(c *CallEvent) (llmtypes.Attempt, bool) {
	if c == nil || len(c.Attempts) == 0 {
		return llmtypes.Attempt{}, false
	}
	return c.Attempts[len(c.Attempts)-1], true
}

func FinishCallFromAudit(c *CallEvent, audit *AuditEvent) {
	if c == nil || audit == nil {
		return
	}
	c.DurationMS = audit.DurationMS
	c.StatusCode = audit.StatusCode
	c.Kind = audit.Kind
}

func AdoptRouteResult(c *CallEvent, result *llmrouter.RouteResult) {
	if c == nil || result == nil {
		return
	}
	c.Attempts = result.Attempts
	c.Vendor = result.Vendor
	c.ModelUsed = result.ModelUsed
}

func AdoptResponse(c *CallEvent, resp *llmtypes.Response, responseBytes int64) {
	if c == nil {
		return
	}
	c.ResponseBytes = responseBytes
	if resp == nil {
		return
	}
	c.Usage = resp.Usage
	if cost, ok := resp.Extra["cost"]; ok && len(cost) > 0 {
		c.VendorCost = string(cost)
	}
}

func SetCallKind(c *CallEvent, kind llmtypes.ErrorKind) {
	if c != nil {
		c.Kind = kind
	}
}

func AdoptStreamSummary(c *CallEvent, sum *llmtypes.Summary, now time.Time) {
	if c == nil {
		return
	}
	if len(c.Attempts) > 0 {
		last := &c.Attempts[len(c.Attempts)-1]
		last.DurationMS = now.Sub(last.StartedAt).Milliseconds()
		if last.Kind == "" && c.Kind != "" {
			last.Kind = c.Kind
		}
		if sum != nil {
			if sum.Usage != nil {
				last.Usage = sum.Usage
			}
			if sum.VendorCost != "" {
				last.VendorCost = sum.VendorCost
			}
		}
	}
	if sum == nil {
		return
	}
	if sum.Usage != nil {
		c.Usage = sum.Usage
	}
	if sum.VendorCost != "" {
		c.VendorCost = sum.VendorCost
	}
}
