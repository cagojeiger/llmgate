package systemone

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/telemetry"
	"llmgate/internal/platform/upstream"
)

const (
	maxTypeSafeRequestBytes = 32 << 20
	maxResponseBytes        = 10 << 20
	statusOverloaded        = 529
)

type decisionRequest struct {
	body       json.RawMessage
	questions  map[string]json.RawMessage
	inputBytes int64
}

type requestProblem struct {
	status  int
	message string
}

func decodeRequest(w http.ResponseWriter, r *http.Request, limit int64) (decisionRequest, *requestProblem) {
	if encoding := r.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return decisionRequest{}, &requestProblem{http.StatusUnsupportedMediaType, "request content encoding is unsupported"}
	}
	if contentType := r.Header.Get("Content-Type"); contentType != "" {
		if mediaType, _, err := mime.ParseMediaType(contentType); err != nil || mediaType != "application/json" {
			return decisionRequest{}, &requestProblem{http.StatusUnsupportedMediaType, "request content type must be application/json"}
		}
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			return decisionRequest{}, &requestProblem{http.StatusRequestEntityTooLarge, "request body exceeds the limit"}
		}
		return decisionRequest{}, &requestProblem{http.StatusBadRequest, "could not read request body"}
	}
	var fields map[string]json.RawMessage
	if !utf8.Valid(body) || json.Unmarshal(body, &fields) != nil || fields == nil {
		return decisionRequest{}, &requestProblem{http.StatusBadRequest, "request body must be a UTF-8 JSON object"}
	}
	var model string
	if json.Unmarshal(fields["model"], &model) != nil || model != Model {
		return decisionRequest{}, &requestProblem{http.StatusBadRequest, "model must be " + Model}
	}
	state := bytes.TrimSpace(fields["state"])
	if len(state) == 0 || (state[0] != '"' && state[0] != '{' && state[0] != '[') {
		return decisionRequest{}, &requestProblem{http.StatusUnprocessableEntity, "state must be a string, object, or array"}
	}
	var questions map[string]json.RawMessage
	if json.Unmarshal(fields["questions"], &questions) != nil || len(questions) == 0 {
		return decisionRequest{}, &requestProblem{http.StatusUnprocessableEntity, "questions must be a nonempty object"}
	}
	for name, question := range questions {
		if len(question) == 0 || question[0] != '{' || name == "" {
			return decisionRequest{}, &requestProblem{http.StatusUnprocessableEntity, "each named question must be an object"}
		}
	}
	if len(fields) != 3 {
		return decisionRequest{}, &requestProblem{http.StatusUnprocessableEntity, "only model, state, and questions are supported"}
	}
	// Send the same model we authorized even if the input repeats a JSON key.
	wire, err := json.Marshal(fields)
	if err != nil {
		return decisionRequest{}, &requestProblem{http.StatusBadRequest, "could not encode request body"}
	}
	return decisionRequest{body: wire, questions: questions, inputBytes: int64(len(body))}, nil
}

func (h *Handler) evaluate(w http.ResponseWriter, ctx context.Context, rec *telemetry.AuditEvent, call *telemetry.CallEvent, input decisionRequest) json.RawMessage {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.cfg.Endpoint, bytes.NewReader(input.body))
	if err != nil {
		fail(w, rec, http.StatusBadGateway, llmtypes.KindUpstream, "TypeSafe is unavailable")
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.cfg.APIKey)

	started := time.Now()
	call.Vendor, call.ModelUsed = vendor, Model
	call.Attempts = []llmtypes.Attempt{{Vendor: vendor, Model: Model, StartedAt: started}}
	attempt := &call.Attempts[0]
	defer func() { attempt.DurationMS = time.Since(started).Milliseconds() }()

	res, err := h.client.Do(req)
	if err != nil {
		failTransport(w, rec, attempt, ctx, err)
		return nil
	}
	defer res.Body.Close()
	attempt.StatusCode = res.StatusCode
	if res.StatusCode != http.StatusOK {
		status, kind, message := providerProblem(res.StatusCode)
		attempt.Kind = kind
		if status == http.StatusTooManyRequests || status == statusOverloaded {
			if retry := upstream.ParseRetryAfter(res.Header.Get("Retry-After")); retry > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retry.Seconds()))))
			}
		}
		fail(w, rec, status, kind, message)
		return nil
	}
	// Limit the decoded body too: the transport may transparently decompress it.
	body, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	if err != nil {
		failTransport(w, rec, attempt, ctx, err)
		return nil
	}
	model, usage := decodeResponse(body, input.questions)
	if usage == nil {
		attempt.Kind = llmtypes.KindUpstream
		fail(w, rec, http.StatusBadGateway, attempt.Kind, "invalid TypeSafe response")
		return nil
	}
	attempt.Usage, call.Usage = usage, usage
	call.ModelUsed, call.ResponseBytes = model, int64(len(body))
	return body
}

func decodeResponse(body []byte, questions map[string]json.RawMessage) (string, *llmtypes.Usage) {
	if len(body) > maxResponseBytes || !utf8.Valid(body) {
		return "", nil
	}
	var result struct {
		Model   string `json:"model"`
		Answers map[string]struct {
			Type string `json:"type"`
		} `json:"answers"`
		Usage struct {
			InputTokens  *int `json:"input_tokens"`
			OutputTokens *int `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &result) != nil || strings.TrimSpace(result.Model) == "" || len(result.Answers) != len(questions) {
		return "", nil
	}
	for name, raw := range questions {
		var question struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &question) != nil || question.Type == "" || result.Answers[name].Type != question.Type {
			return "", nil
		}
	}
	if result.Usage.InputTokens == nil || result.Usage.OutputTokens == nil {
		return "", nil
	}
	input, output := *result.Usage.InputTokens, *result.Usage.OutputTokens
	if input < 0 || output < 0 || output > math.MaxInt-input {
		return "", nil
	}
	return result.Model, &llmtypes.Usage{PromptTokens: input, CompletionTokens: output, TotalTokens: input + output}
}

func providerProblem(status int) (int, llmtypes.ErrorKind, string) {
	switch status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
		return status, llmtypes.KindBadRequest, "TypeSafe rejected the request"
	case http.StatusTooManyRequests:
		return status, llmtypes.KindRateLimit, "TypeSafe rate limit exceeded"
	case statusOverloaded:
		return status, llmtypes.KindUpstream, "TypeSafe is overloaded"
	default:
		return http.StatusBadGateway, llmtypes.KindUpstream, "TypeSafe is unavailable"
	}
}

func failTransport(w http.ResponseWriter, rec *telemetry.AuditEvent, attempt *llmtypes.Attempt, ctx context.Context, err error) {
	if errors.Is(ctx.Err(), context.Canceled) {
		attempt.Kind = llmtypes.KindClientClosed
		fail(w, rec, 499, attempt.Kind, "client closed the request")
		return
	}
	attempt.Kind = upstream.LowLevelError(vendor, "TypeSafe request failed", err).Kind
	message := "TypeSafe is unavailable"
	if attempt.Kind == llmtypes.KindTimeout {
		message = "TypeSafe request timed out"
	}
	fail(w, rec, http.StatusBadGateway, attempt.Kind, message)
}
