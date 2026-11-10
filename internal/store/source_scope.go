package store

import (
	"context"
	"errors"

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

func (s *Store) incomingScopeClause(ctx context.Context, message, source string) (string, []any) {
	predicate, args := sourcescope.IncomingPredicate(ctx, message, source, s.dialect.UnicodeLowerExpression)
	return " AND " + predicate, args
}

// IncomingMessageAllowed checks the scoped From snapshots of one message.
func (s *Store) IncomingMessageAllowed(ctx context.Context, id int64) (bool, error) {
	scope, args := s.incomingScopeClause(ctx, "m.id", "m.source_id")
	args = append([]any{id}, args...)
	rows, err := s.db.QueryContext(ctx, "SELECT m.id FROM messages m WHERE m.id = ?"+scope, args...)
	if err != nil {
		return false, err
	}
	allowed := rows.Next()
	err = rows.Err()
	closeErr := rows.Close()
	return allowed, errors.Join(err, closeErr)
}
