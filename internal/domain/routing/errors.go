package routing

import (
	"context"
	"errors"

	"llmgate/internal/domain/llmtypes"
)

// contextError reclassifies a ctx.Err() so callers surface KindTimeout
// instead of a bare context.DeadlineExceeded.
func contextError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &llmtypes.Error{Kind: llmtypes.KindTimeout, Message: err.Error(), Cause: err}
	}
	return err
}
