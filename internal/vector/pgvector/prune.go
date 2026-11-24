package pgvector

import (
	"context"
	"database/sql"
	"fmt"
)

// PruneOrphanEmbeddings removes message embeddings whose authoritative
// message row has been hard-deleted. Generation row locks serialize pruning
// with the backend's other embedding writers, so embeddings and message_count
// are repaired atomically without racing an upsert or retirement.
func PruneOrphanEmbeddings(ctx context.Context, db *sql.DB) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin orphan prune: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// The count, delete, and count repair below are corpus-wide maintenance.
	// Disable the pool's 30-second statement timeout for this transaction so
	// a large accumulated orphan set can complete. SET LOCAL resets when the
	// transaction commits or rolls back.
	if _, err := tx.ExecContext(ctx, "SET LOCAL statement_timeout = 0"); err != nil {
		return 0, fmt.Errorf("disable statement_timeout for orphan prune: %w", err)
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM index_generations ORDER BY id FOR UPDATE`)
	if err != nil {
		return 0, fmt.Errorf("lock generations for orphan prune: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var generationID int64
		if err := rows.Scan(&generationID); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan locked generation: %w", err)
		}
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close locked generations: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate locked generations: %w", err)
	}

	var pruned int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM (
			SELECT e.generation_id, e.message_id
			  FROM embeddings e
			 WHERE NOT EXISTS (SELECT 1 FROM messages m WHERE m.id = e.message_id)
			 GROUP BY e.generation_id, e.message_id
		) orphan_embeddings`).Scan(&pruned); err != nil {
		return 0, fmt.Errorf("count orphan embeddings: %w", err)
	}
	if pruned > 0 {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM embeddings e
			 WHERE NOT EXISTS (SELECT 1 FROM messages m WHERE m.id = e.message_id)`); err != nil {
			return 0, fmt.Errorf("delete orphan embeddings: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE index_generations g
			   SET message_count = (
				SELECT COUNT(DISTINCT e.message_id)
				  FROM embeddings e
				 WHERE e.generation_id = g.id
			   )`); err != nil {
			return 0, fmt.Errorf("repair generation message counts: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit orphan prune: %w", err)
	}
	return pruned, nil
}
