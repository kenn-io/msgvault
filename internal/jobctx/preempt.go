package jobctx

import (
	"context"
	"sync/atomic"
)

type preemptionKey struct{}

type preemptionRequest struct {
	requested atomic.Bool
	signal    chan struct{}
}

// WithPreemption returns a context that carries a cooperative preemption
// request and the function that raises it. Raising it never cancels the
// context: a resumable job that checks PreemptionRequested stops at its next
// safe boundary and returns without a failure. The scheduler then queues a
// follow-up, including when the callback returns nil.
func WithPreemption(ctx context.Context) (context.Context, func()) {
	request := &preemptionRequest{signal: make(chan struct{})}
	return context.WithValue(ctx, preemptionKey{}, request), func() {
		if request.requested.CompareAndSwap(false, true) {
			close(request.signal)
		}
	}
}

// PreemptionRequested reports whether the scheduler asked the job running
// under ctx to yield to queued work.
func PreemptionRequested(ctx context.Context) bool {
	request, ok := ctx.Value(preemptionKey{}).(*preemptionRequest)
	return ok && request.requested.Load()
}

// PreemptionSignal returns a channel that closes when a cooperative
// preemption is requested. It returns nil when ctx carries no request.
func PreemptionSignal(ctx context.Context) <-chan struct{} {
	request, ok := ctx.Value(preemptionKey{}).(*preemptionRequest)
	if !ok {
		return nil
	}
	return request.signal
}

// StopRequested reports cancellation or a cooperative request to yield.
func StopRequested(ctx context.Context) bool {
	return ctx.Err() != nil || PreemptionRequested(ctx)
}
