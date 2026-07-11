package chat

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"llmgate/internal/domain/llmtypes"
)

// defaultMaxChatRequestBytes sizes the body cap for base64 image content
// (a phone photo is 2-5 MB, +33% base64); mirrors the config default and
// covers callers that construct the handler without a configured cap.
const defaultMaxChatRequestBytes = 10 << 20

func decodeChatRequest(w http.ResponseWriter, r *http.Request, maxBytes int64) (*llmtypes.Request, int64, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, 0, &llmtypes.Error{
				Kind:    llmtypes.KindBadRequest,
				Message: fmt.Sprintf("request body exceeds %d bytes (LLMGATE_MAX_REQUEST_BYTES)", tooLarge.Limit),
			}
		}
		return nil, 0, &llmtypes.Error{Kind: llmtypes.KindBadRequest, Message: "read request body: " + err.Error()}
	}
	req := &llmtypes.Request{}
	if err := json.Unmarshal(body, req); err != nil {
		return nil, int64(len(body)), &llmtypes.Error{Kind: llmtypes.KindBadRequest, Message: "decode request: " + err.Error()}
	}
	return req, int64(len(body)), nil
}

func modelAllowed(model string, allowed []string) bool {
	if model == "" || len(allowed) == 0 {
		return true
	}
	for _, alias := range allowed {
		if strings.EqualFold(model, alias) {
			return true
		}
	}
	return false
}

func modelNotAllowedError() *llmtypes.Error {
	return &llmtypes.Error{Kind: llmtypes.KindForbidden, Message: "model not allowed"}
}
