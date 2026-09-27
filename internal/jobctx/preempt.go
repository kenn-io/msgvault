package jobctx

import (
	"context"
	"sync/atomic"
)

type preemptionKey struct{}

// WithPreemption returns a context that carries a cooperative preemption
// request and the function that raises it. Raising it never cancels the
// context: a resumable job that checks PreemptionRequested stops at its next
// safe boundary and completes normally, so the run is not recorded as failed.
func WithPreemption(ctx context.Context) (context.Context, func()) {
	flag := new(atomic.Bool)
	return context.WithValue(ctx, preemptionKey{}, flag), func() { flag.Store(true) }
}

// PreemptionRequested reports whether the scheduler asked the job running
// under ctx to yield to queued work.
func PreemptionRequested(ctx context.Context) bool {
	flag, ok := ctx.Value(preemptionKey{}).(*atomic.Bool)
	return ok && flag.Load()
}
