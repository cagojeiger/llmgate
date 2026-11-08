package systemone

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/klauspost/compress/zstd"

	"llmgate/internal/domain/llmtypes"
	"llmgate/internal/domain/telemetry"
	"llmgate/internal/platform/upstream"
)

const (
	maxTypeSafeRequestBytes = 32 << 20
	maxResponseBytes        = 10 << 20
	statusOverloaded        = 529
	maxZstdWindow           = 8 << 20
)

type decisionRequest struct {
	body        []byte
	questions   map[string]json.RawMessage
	contentType string
	headers     map[string]string
	inputBytes  int64
}

type requestProblem struct {
	status  int
	kind    llmtypes.ErrorKind
	message string
}

func decodeRequest(w http.ResponseWriter, r *http.Request, limit int64) (decisionRequest, *requestProblem) {
	mediaType := "application/json"
	var parameters map[string]string
	if contentType := r.Header.Get("Content-Type"); contentType != "" {
		var err error
		mediaType, parameters, err = mime.ParseMediaType(contentType)
		if err != nil || (mediaType != "application/json" && mediaType != "multipart/form-data") {
			return decisionRequest{}, &requestProblem{http.StatusUnsupportedMediaType, llmtypes.KindBadRequest, "request content type must be application/json or multipart/form-data"}
		}
	}

	body, problem := readRequestBody(w, r, limit)
	if problem != nil {
		return decisionRequest{}, problem
	}
	jsonBody := body
	var parts *multipart.Reader
	boundary := parameters["boundary"]
	if mediaType == "multipart/form-data" {
		var err error
		jsonBody, parts, err = readMultipart(body, boundary)
		if err != nil {
			return decisionRequest{}, &requestProblem{http.StatusBadRequest, llmtypes.KindBadRequest, "invalid multipart request"}
		}
	}
	var fields map[string]json.RawMessage
	if !utf8.Valid(jsonBody) || json.Unmarshal(jsonBody, &fields) != nil || fields == nil {
		return decisionRequest{}, &requestProblem{http.StatusBadRequest, llmtypes.KindBadRequest, "request body must be a UTF-8 JSON object"}
	}
	var model string
	if json.Unmarshal(fields["model"], &model) != nil || model != Model {
		return decisionRequest{}, &requestProblem{http.StatusBadRequest, llmtypes.KindBadRequest, "model must be " + Model}
	}
	state := bytes.TrimSpace(fields["state"])
	if len(state) == 0 || (state[0] != '"' && state[0] != '{' && state[0] != '[') {
		return decisionRequest{}, &requestProblem{http.StatusUnprocessableEntity, llmtypes.KindBadRequest, "state must be a string, object, or array"}
	}
	var questions map[string]json.RawMessage
	if json.Unmarshal(fields["questions"], &questions) != nil || len(questions) == 0 {
		return decisionRequest{}, &requestProblem{http.StatusUnprocessableEntity, llmtypes.KindBadRequest, "questions must be a nonempty object"}
	}
	for name, question := range questions {
		if len(question) == 0 || question[0] != '{' || name == "" {
			return decisionRequest{}, &requestProblem{http.StatusUnprocessableEntity, llmtypes.KindBadRequest, "each named question must be an object"}
		}
	}
	for key := range fields {
		switch key {
		case "model", "state", "questions", "assets":
		default:
			return decisionRequest{}, &requestProblem{http.StatusUnprocessableEntity, llmtypes.KindBadRequest, "unsupported System One request field"}
		}
	}
	// Send the same model we authorized even if the input repeats a JSON key.
	wire, err := json.Marshal(fields)
	if err != nil {
		return decisionRequest{}, &requestProblem{http.StatusBadRequest, llmtypes.KindBadRequest, "could not encode request body"}
	}
	contentType := mediaType
	if mediaType == "multipart/form-data" {
		wire, err = encodeMultipart(boundary, wire, fields["assets"], parts)
		if err != nil {
			return decisionRequest{}, &requestProblem{http.StatusBadRequest, llmtypes.KindBadRequest, "could not encode multipart request"}
		}
		contentType = mime.FormatMediaType(mediaType, map[string]string{"boundary": boundary})
	}
	if len(wire) > maxTypeSafeRequestBytes {
		return decisionRequest{}, &requestProblem{http.StatusRequestEntityTooLarge, llmtypes.KindBadRequest, "request body exceeds the limit"}
	}
	return decisionRequest{
		body: wire, questions: questions, contentType: contentType,
		headers: featureHeaders(r.Header), inputBytes: int64(len(body)),
	}, nil
}

func readRequestBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, *requestProblem) {
	var reader io.Reader = http.MaxBytesReader(w, r.Body, limit)
	encoding := strings.ToLower(strings.TrimSpace(strings.Join(r.Header.Values("Content-Encoding"), ",")))
	switch encoding {
	case "", "identity":
	case "gzip", "x-gzip":
		decoded, err := gzip.NewReader(reader)
		if err != nil {
			return nil, bodyReadProblem(err)
		}
		defer decoded.Close()
		reader = decoded
	case "zstd":
		decoded, err := zstd.NewReader(reader, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxWindow(maxZstdWindow))
		if err != nil {
			return nil, bodyReadProblem(err)
		}
		defer decoded.Close()
		reader = decoded
	default:
		return nil, &requestProblem{http.StatusUnsupportedMediaType, llmtypes.KindBadRequest, "request content encoding is unsupported"}
	}
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, bodyReadProblem(err)
	}
	if int64(len(body)) > limit {
		return nil, bodyReadProblem(&http.MaxBytesError{Limit: limit})
	}
	return body, nil
}

func bodyReadProblem(err error) *requestProblem {
	var large *http.MaxBytesError
	if errors.As(err, &large) {
		return &requestProblem{http.StatusRequestEntityTooLarge, llmtypes.KindBadRequest, "request body exceeds the limit"}
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return &requestProblem{http.StatusBadGateway, llmtypes.KindTimeout, "request timed out"}
	}
	return &requestProblem{http.StatusBadRequest, llmtypes.KindBadRequest, "could not read request body"}
}

func featureHeaders(headers http.Header) map[string]string {
	out := make(map[string]string)
	for _, name := range []string{"TypeSafe-Danger-Zone-Images", "TypeSafe-Danger-Zone-Long-Context"} {
		if values := headers.Values(name); len(values) == 1 && values[0] == "true" {
			out[http.CanonicalHeaderKey(name)] = "true"
		}
	}
	return out
}

func (r decisionRequest) auditBody() json.RawMessage {
	if r.contentType == "application/json" {
		return r.body
	}
	// JSONL needs a binary-safe representation of the forwarded MIME body.
	body, _ := json.Marshal(struct {
		ContentType string `json:"content_type"`
		Body        []byte `json:"body_base64"`
	}{r.contentType, r.body})
	return body
}

func (h *Handler) evaluate(w http.ResponseWriter, ctx context.Context, rec *telemetry.AuditEvent, call *telemetry.CallEvent, input decisionRequest) json.RawMessage {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.cfg.Endpoint, bytes.NewReader(input.body))
	if err != nil {
		fail(w, rec, http.StatusBadGateway, llmtypes.KindUpstream, "TypeSafe is unavailable")
		return nil
	}
	req.Header.Set("Content-Type", input.contentType)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.cfg.APIKey)
	for name, value := range input.headers {
		req.Header.Set(name, value)
	}

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
		Model   string                     `json:"model"`
		Answers map[string]json.RawMessage `json:"answers"`
		Usage   struct {
			InputTokens  *int `json:"input_tokens"`
			OutputTokens *int `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &result) != nil || strings.TrimSpace(result.Model) == "" || len(result.Answers) != len(questions) {
		return "", nil
	}
	for name, raw := range questions {
		var question, answer struct {
			Type string `json:"type"`
		}
		rawAnswer := result.Answers[name]
		if json.Unmarshal(raw, &question) != nil || question.Type == "" ||
			json.Unmarshal(rawAnswer, &answer) != nil || answer.Type != question.Type {
			return "", nil
		}
		if answer.Type == "noul" {
			// A map enforces the exact key spelling that clients read from the raw response.
			var fields map[string]json.RawMessage
			var probability *float64
			if json.Unmarshal(rawAnswer, &fields) != nil || json.Unmarshal(fields["noul"], &probability) != nil ||
				probability == nil || *probability < 0 || *probability > 1 {
				return "", nil
			}
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
