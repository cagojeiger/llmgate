package response

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"time"

	"llmgate/internal/domain/llmtypes"
)

func WriteError(w http.ResponseWriter, err error) {
	status, retryAfter, payload := errorPayload(err)
	w.Header().Set("Content-Type", "application/json")
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.FormatInt(int64(math.Ceil(retryAfter.Seconds())), 10))
	}
	w.WriteHeader(status)
	_, _ = w.Write(append(payload, '\n'))
}

func Status(err error) int {
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
		case llmtypes.KindForbidden:
			status = http.StatusForbidden
		case llmtypes.KindRateLimit:
			status = http.StatusTooManyRequests
		case llmtypes.KindBadRequest:
			status = http.StatusBadRequest
		case llmtypes.KindContextLength:
			status, code = http.StatusBadRequest, "context_length_exceeded"
		case llmtypes.KindContentFilter:
			status, code = http.StatusBadRequest, "content_filter"
		case llmtypes.KindNetwork, llmtypes.KindTimeout, llmtypes.KindEmpty:
			// Low-level transport faults wrap a Cause that may include
			// IPs, hostnames, or DNS detail. Adapter diagnostics on these
			// kinds have no Cause and already carry public messages.
			status = http.StatusBadGateway
			if llmtypes.CauseOf(err) != nil {
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
