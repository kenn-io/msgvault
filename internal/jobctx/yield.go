package jobctx

import (
	"context"
	"errors"
)

// ErrYieldedToWaiter is the cancellation cause used when a resumable job
// steps aside so a waiting operation can acquire the work gate.
var ErrYieldedToWaiter = errors.New("yielded to a waiting operation")

// YieldedToWaiter reports whether ctx was cancelled because a job yielded to
// a waiting operation.
func YieldedToWaiter(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), ErrYieldedToWaiter)
}
