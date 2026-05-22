package embed

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"go.kenn.io/msgvault/internal/vector"
)

// Queue wraps pending_embeddings with a crash-safe claim-mark-complete
// pattern. A claim atomically marks up to N available rows with a token
// and the current timestamp; Complete deletes the rows (on success) and
// Release clears the claim (on failure). Rows whose claims are older
// than a configurable cutoff can be reclaimed via ReclaimStale, so a
// crashed worker does not strand pending work.
type Queue struct {
	db *sql.DB
}

// NewQueue returns a Queue bound to db. The caller retains ownership of
// db; Queue does not close it.
func NewQueue(db *sql.DB) *Queue { return &Queue{db: db} }

// Claim marks up to batch pending rows for gen as claimed by a fresh
// token, returning the message IDs in ascending order alongside the
// token to present to Complete or Release.
//
// If batch <= 0, or no rows are available, Claim returns (nil, "", nil).
// Returning an empty token for "no work" avoids asking callers to hold a
// dead token.
func (q *Queue) Claim(ctx context.Context, gen vector.GenerationID, batch int) ([]int64, string, error) {
	if batch <= 0 {
		return nil, "", nil
	}
	token, err := newToken()
	if err != nil {
		return nil, "", fmt.Errorf("new token: %w", err)
	}
	now := time.Now().Unix()

	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", fmt.Errorf("begin claim tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
        UPDATE pending_embeddings
           SET claimed_at = ?, claim_token = ?
         WHERE (generation_id, message_id) IN (
               SELECT generation_id, message_id
                 FROM pending_embeddings
                WHERE generation_id = ?
                  AND claimed_at IS NULL
                ORDER BY message_id
                LIMIT ?)
        RETURNING message_id`,
		now, token, int64(gen), batch)
	if err != nil {
		return nil, "", fmt.Errorf("claim query: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, "", fmt.Errorf("scan claimed id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, "", fmt.Errorf("claim rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, "", fmt.Errorf("close claim rows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, "", fmt.Errorf("commit claim: %w", err)
	}
	if len(ids) == 0 {
		return nil, "", nil
	}
	// The subquery's ORDER BY decides WHICH rows get claimed, but
	// SQLite does not guarantee RETURNING yields them in that order.
	// Sort explicitly so callers can rely on ascending ids (matters
	// for deterministic test assertions and for pairing ids with
	// fetched message bodies by position).
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, token, nil
}

// Complete deletes the claimed rows from the queue. Only rows whose
// claim_token matches token are removed; any row that was reclaimed or
// re-claimed under a different token is left in place. A nil or empty
// ids slice is a no-op.
func (q *Queue) Complete(ctx context.Context, gen vector.GenerationID, token string, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	blob, err := json.Marshal(ids)
	if err != nil {
		return fmt.Errorf("encode ids: %w", err)
	}
	if _, err := q.db.ExecContext(ctx, `
        DELETE FROM pending_embeddings
         WHERE generation_id = ?
           AND claim_token   = ?
           AND message_id IN (SELECT value FROM json_each(?))`,
		int64(gen), token, string(blob)); err != nil {
		return fmt.Errorf("delete pending: %w", err)
	}
	return nil
}

// Release returns claimed rows to the pool so another worker can pick
// them up (for embedding failures). Only rows whose claim_token matches
// token are released. A nil or empty ids slice is a no-op.
func (q *Queue) Release(ctx context.Context, gen vector.GenerationID, token string, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	blob, err := json.Marshal(ids)
	if err != nil {
		return fmt.Errorf("encode ids: %w", err)
	}
	if _, err := q.db.ExecContext(ctx, `
        UPDATE pending_embeddings
           SET claimed_at = NULL, claim_token = NULL
         WHERE generation_id = ?
           AND claim_token   = ?
           AND message_id IN (SELECT value FROM json_each(?))`,
		int64(gen), token, string(blob)); err != nil {
		return fmt.Errorf("release: %w", err)
	}
	return nil
}

// ReclaimStale clears the claim on any pending row whose claimed_at is
// older than olderThan. Returns the number of rows reclaimed.
func (q *Queue) ReclaimStale(ctx context.Context, olderThan time.Duration) (int, error) {
	cutoff := time.Now().Add(-olderThan).Unix()
	res, err := q.db.ExecContext(ctx, `
        UPDATE pending_embeddings
           SET claimed_at = NULL, claim_token = NULL
         WHERE claimed_at IS NOT NULL AND claimed_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("reclaim stale: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return int(n), nil
}

// newToken returns 16 hex characters backed by 8 bytes of crypto/rand.
func newToken() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return hex.EncodeToString(b), nil
}
