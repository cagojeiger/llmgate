package server

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"time"

	"llmgate/internal/llmtypes"
)

func writeError(w http.ResponseWriter, err error) {
	status, retryAfter, payload := errorPayload(err)
	w.Header().Set("Content-Type", "application/json")
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.FormatInt(int64(math.Ceil(retryAfter.Seconds())), 10))
	}
	w.WriteHeader(status)
	_, _ = w.Write(append(payload, '\n'))
}

func errStatus(err error) int {
	status, _, _ := errorPayload(err)
	return status
}

// errorPayload classifies err into HTTP status / OpenAI-style error
// envelope / Retry-After hint. Non-provider errors degrade to 500/unknown
// so handler call sites never have to special-case nil or wrapped errors.
func errorPayload(err error) (int, time.Duration, []byte) {
	status := http.StatusInternalServerError
	kind := llmtypes.ErrorKindOf(err)
	if kind == "" {
		kind = llmtypes.KindUnknown
	}
	message := llmtypes.MessageOf(err)
	if message == "" {
		message = "unknown error"
	}
	var code any
	retryAfter := llmtypes.RetryAfterOf(err)

	transportClass := false
	if err != nil {
		switch kind {
		case llmtypes.KindAuth:
			status = http.StatusUnauthorized
		case llmtypes.KindRateLimit:
			status = http.StatusTooManyRequests
		case llmtypes.KindBadRequest:
			status = http.StatusBadRequest
		case llmtypes.KindContextLength:
			status, code = http.StatusBadRequest, "context_length_exceeded"
		case llmtypes.KindContentFilter:
			status, code = http.StatusBadRequest, "content_filter"
		case llmtypes.KindNetwork, llmtypes.KindTimeout, llmtypes.KindEmpty:
			// Two distinct sources land on these kinds:
			//   1. Low-level transport faults via upstream/http.go's
			//      LowLevelError or sse_reader's scanner.Err(). Message
			//      is built from cause.Error() and may carry upstream
			//      IPs, in-cluster hostnames, or DNS detail. Cause is
			//      set; StatusCode is 0 (no upstream HTTP response).
			//   2. Adapter-classified vendor responses (HTTP 408 →
			//      KindTimeout, 502/504 → KindEmpty/KindNetwork by
			//      vendor mapping). Message is parsed from the vendor
			//      envelope, already vendor-shaped and safe to surface.
			//      StatusCode is preserved (408 / 502 / 504 / ...).
			//
			// Sanitize only the transport branch (StatusCode == 0).
			// The adapter branch keeps its diagnostic so callers retain
			// the vendor's envelope message; upstream status surfacing
			// to the wire is a separate decision for a later PR.
			//
			// KindUpstream is also intentionally NOT collapsed here:
			// that kind is always set by provider adapters with
			// deliberately-shaped messages — it never originates from
			// the transport layer.
			status = http.StatusBadGateway
			if llmtypes.StatusCodeOf(err) == 0 {
				transportClass = true
				if kind == llmtypes.KindTimeout {
					message = "upstream timeout"
				} else {
					message = "upstream unavailable"
				}
			}
		case llmtypes.KindUpstream:
			status = http.StatusBadGateway
		case llmtypes.KindClientClosed:
			status = 499
		}
		if !transportClass && llmtypes.MessageOf(err) == "" {
			message = http.StatusText(status)
		}
	}

	out, marshalErr := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    string(kind),
			"code":    code,
			"status":  status,
		},
	})
	if marshalErr != nil {
		return http.StatusInternalServerError, 0, []byte(`{"error":{"message":"encode error","type":"unknown","code":null,"status":500}}`)
	}
	return status, retryAfter, out
}
