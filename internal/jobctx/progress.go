package jobctx

import (
	"context"
	"errors"
	"sync/atomic"
)

// ErrRunBudgetExceeded is the cause of a scheduled pass's runtime deadline.
var ErrRunBudgetExceeded = errors.New("scheduled job runtime budget exceeded")

type progressKey struct{}

// WithProgress tracks committed checkpoints within one scheduled pass.
func WithProgress(ctx context.Context) context.Context {
	return context.WithValue(ctx, progressKey{}, new(atomic.Bool))
}

// RecordProgress records a committed checkpoint that a later pass can resume.
func RecordProgress(ctx context.Context) {
	if progress, ok := ctx.Value(progressKey{}).(*atomic.Bool); ok {
		progress.Store(true)
	}
}

// HasProgress reports whether this pass committed a checkpoint.
func HasProgress(ctx context.Context) bool {
	progress, ok := ctx.Value(progressKey{}).(*atomic.Bool)
	return ok && progress.Load()
}
