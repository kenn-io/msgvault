package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
)

// XContentQuerier allows the vector transaction to check suppression after its
// generation lock, without another transaction or an archive connection pool wait.
type XContentQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func xContentTableExists(ctx context.Context, q XContentQuerier, postgres bool) (bool, error) {
	if q == nil {
		return false, nil
	}
	var exists bool
	sql := `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='x_post_versions')`
	if postgres {
		sql = `SELECT to_regclass('x_post_versions') IS NOT NULL`
	}
	err := q.QueryRowContext(ctx, sql).Scan(&exists)
	return exists, err
}

// XContentCleanupPendingContext also serves already-open Parquet readers. Small
// standalone query/vector databases without the main archive schema have no X data.
func XContentCleanupPendingContext(ctx context.Context, q XContentQuerier, postgres bool) (bool, error) {
	exists, err := xContentTableExists(ctx, q, postgres)
	if err != nil || !exists {
		return false, err
	}
	var pending bool
	err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM x_post_versions WHERE cleanup_pending=1)`).Scan(&pending)
	return pending, err
}

// XSuppressedMessagesContext fences stale embedding results against durable
// suppression. Call after taking the vector generation write lock; cleanup's
// Delete takes the same lock, so a prior write is deleted and a later one refused.
func XSuppressedMessagesContext(ctx context.Context, q XContentQuerier, postgres bool, ids []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	exists, err := xContentTableExists(ctx, q, postgres)
	if err != nil || !exists {
		return out, err
	}
	data, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	sql := `SELECT message_id FROM x_post_versions WHERE suppressed_at IS NOT NULL AND message_id IN (SELECT CAST(value AS BIGINT) FROM json_each(?)) GROUP BY message_id`
	if postgres {
		sql = `SELECT message_id FROM x_post_versions WHERE suppressed_at IS NOT NULL AND message_id IN (SELECT CAST(value AS BIGINT) FROM jsonb_array_elements_text($1::jsonb)) GROUP BY message_id`
	}
	rows, err := q.QueryContext(ctx, sql, string(data))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
