package server

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"llmgate/internal/provider"
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
	kind := provider.KindUnknown
	message := "unknown error"
	var code any
	var retryAfter time.Duration

	var perr *provider.Error
	if errors.As(err, &perr) {
		kind = perr.Kind
		retryAfter = perr.RetryAfter
		message = perr.Message
		switch perr.Kind {
		case provider.KindAuth:
			status = http.StatusUnauthorized
		case provider.KindRateLimit:
			status = http.StatusTooManyRequests
		case provider.KindBadRequest:
			status = http.StatusBadRequest
		case provider.KindContextLength:
			status, code = http.StatusBadRequest, "context_length_exceeded"
		case provider.KindContentFilter:
			status, code = http.StatusBadRequest, "content_filter"
		case provider.KindUpstream, provider.KindNetwork, provider.KindTimeout, provider.KindEmpty:
			status = http.StatusBadGateway
		case provider.KindClientClosed:
			status = 499
		}
		if kind == "" {
			kind = provider.KindUnknown
		}
		if message == "" {
			message = http.StatusText(status)
		}
	} else if err != nil {
		message = err.Error()
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
