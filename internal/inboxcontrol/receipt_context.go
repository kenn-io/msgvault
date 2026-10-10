package inboxcontrol

import "context"

type receiptMoveTargetKey struct{}

// ReceiptMoveTarget returns a destination proved by durable dispatch evidence.
// Only the authenticated receipt path can create this context value; callers
// cannot supply it through a request. It does not bypass archive item binding.
func ReceiptMoveTarget(ctx context.Context) (Target, bool) {
	target, ok := ctx.Value(receiptMoveTargetKey{}).(Target)
	return target, ok
}
