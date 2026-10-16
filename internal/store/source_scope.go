package store

import (
	"context"

	"go.kenn.io/msgvault/internal/sourcescope"
)

// sourceScopeClause is independent of user-selected query filters: those
// filters can narrow authorization, but cannot replace it.
func sourceScopeClause(ctx context.Context, column string) (string, []any) {
	ids := sourcescope.IDs(ctx)
	if ids == nil {
		return "", nil
	}
	if len(ids) == 0 {
		return " AND 1=0", nil
	}
	predicate, args := messageExportInClause(column, ids)
	return " AND " + predicate, args
}
