package query

import "context"

func (e *SQLiteEngine) ListMessagesWithSenderBound(ctx context.Context, filter MessageFilter, bound int) ([]MessageSummary, error) {
	return e.listMessages(ctx, filter, bound)
}
