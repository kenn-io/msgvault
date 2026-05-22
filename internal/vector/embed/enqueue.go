package embed

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/vector"
)

// Enqueuer inserts message IDs into pending_embeddings for every
// non-retired generation. Implements the EmbedEnqueuer interface
// expected by internal/sync.
//
// Dual-enqueue is intentional: when a rebuild is in progress there are
// two non-retired generations (active + building); every newly-synced
// message gets queued into both so the building index stays current.
type Enqueuer struct {
	db *sql.DB
}

// NewEnqueuer returns an Enqueuer backed by vectors.db.
func NewEnqueuer(db *sql.DB) *Enqueuer {
	return &Enqueuer{db: db}
}

// EnqueueMessages adds the given IDs to pending_embeddings for every
// generation not in state 'retired'. Duplicate IDs are silently ignored
// via INSERT OR IGNORE. Caller must only pass non-deleted message IDs —
// the deletion predicate is not checked here.
func (e *Enqueuer) EnqueueMessages(ctx context.Context, messageIDs []int64) error {
	if len(messageIDs) == 0 {
		return nil
	}
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin enqueue tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM index_generations WHERE state != ?`,
		string(vector.GenerationRetired))
	if err != nil {
		return fmt.Errorf("select non-retired generations: %w", err)
	}
	var gens []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan generation id: %w", err)
		}
		gens = append(gens, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate generations: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close generation rows: %w", err)
	}
	if len(gens) == 0 {
		return tx.Commit()
	}

	// Bulk-insert one row per (gen, message) pair via a single statement
	// per generation, expanded with json_each(message_ids). For a 5,000-
	// message incremental batch with two non-retired generations, this
	// is two writes against the vectors.db lock instead of 10,000 — keeps
	// the embed worker's Claim from starving while sync flushes.
	blob, err := json.Marshal(messageIDs)
	if err != nil {
		return fmt.Errorf("encode message ids: %w", err)
	}
	now := time.Now().Unix()
	for _, g := range gens {
		if _, err := tx.ExecContext(ctx, `
            INSERT OR IGNORE INTO pending_embeddings (generation_id, message_id, enqueued_at)
            SELECT ?, value, ? FROM json_each(?)`,
			g, now, string(blob)); err != nil {
			return fmt.Errorf("insert pending (gen=%d): %w", g, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit enqueue: %w", err)
	}
	return nil
}
