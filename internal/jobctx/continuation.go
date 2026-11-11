package jobctx

import "context"

type continuationKey struct{}

// WithContinuation marks ctx as a scheduler follow-up that only resumes a run
// which yielded or asked to continue. No scheduled tick or manual request was
// merged into it.
func WithContinuation(ctx context.Context) context.Context {
	return context.WithValue(ctx, continuationKey{}, true)
}

// Continuation reports whether the run under ctx only resumes earlier work.
// Jobs that keep progress across a yield use it to tell a resumption from a
// new request, which should start over.
func Continuation(ctx context.Context) bool {
	continuation, _ := ctx.Value(continuationKey{}).(bool)
	return continuation
}
