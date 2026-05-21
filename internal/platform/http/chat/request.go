package chat

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"llmgate/internal/domain/llmtypes"
)

const maxChatRequestBytes = 1 << 20

func decodeChatRequest(w http.ResponseWriter, r *http.Request) (*llmtypes.Request, int64, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxChatRequestBytes))
	if err != nil {
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
