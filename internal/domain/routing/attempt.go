package routing

import (
	"context"
	"errors"

	"llmgate/internal/domain/llmtypes"
)

func requestForCandidate(req *llmtypes.Request, candidate candidate) llmtypes.Request {
	attemptReq := *req
	attemptReq.Model = candidate.model
	return attemptReq
}

func adoptAttemptError(att *llmtypes.Attempt, err error) {
	att.Kind = llmtypes.ErrorKindOf(err)
	att.StatusCode = llmtypes.StatusCodeOf(err)
}

func contextError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &llmtypes.Error{Kind: llmtypes.KindTimeout, Message: err.Error(), Cause: err}
	}
	return err
}
