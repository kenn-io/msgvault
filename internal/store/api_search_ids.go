package store

import (
	"context"
	"fmt"

	"go.kenn.io/msgvault/internal/search"
)

// SearchMessageIDsQueryContext resolves a bounded ranked candidate pool without
// hydrating messages, recipients, or labels. Total counts the entire matching
// population so callers can distinguish a complete pool from a truncated one.
func (s *Store) SearchMessageIDsQueryContext(
	ctx context.Context, q *search.Query, limit int,
) ([]int64, int64, error) {
	return s.searchMessageIDsQuery(ctx, q, limit, s.fts5Available)
}

func (s *Store) searchMessageIDsQuery(
	ctx context.Context, q *search.Query, limit int, ftsAvailable bool,
) ([]int64, int64, error) {
	plan := s.buildMessageSearchSQL(q, ftsAvailable)
	from := " FROM messages m " + plan.join + " WHERE " + plan.where
	var total int64
	if err := s.db.QueryRowContext(
		ctx, "SELECT COUNT(*)"+from, plan.args...,
	).Scan(&total); err != nil {
		if plan.fts && ctx.Err() == nil {
			return s.searchMessageIDsQuery(ctx, q, limit, false)
		}
		return nil, 0, fmt.Errorf("count search candidates: %w", err)
	}
	args := append(append([]any{}, plan.args...), plan.orderArgs...)
	args = append(args, limit)
	rows, err := s.db.QueryContext(
		ctx, "SELECT m.id"+from+" ORDER BY "+plan.orderBy+" LIMIT ?", args...,
	)
	if err != nil {
		if plan.fts && ctx.Err() == nil {
			return s.searchMessageIDsQuery(ctx, q, limit, false)
		}
		return nil, 0, fmt.Errorf("query search candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, 0, fmt.Errorf("scan search candidate: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate search candidates: %w", err)
	}
	return ids, total, nil
}
