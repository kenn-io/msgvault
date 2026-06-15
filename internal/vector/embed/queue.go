package embed

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
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
	db     *sql.DB
	rebind func(string) string
	// isPG is true when the underlying driver is PostgreSQL. When set,
	// Claim uses FOR UPDATE SKIP LOCKED in the inner SELECT to prevent
	// concurrent workers from claiming the same pending rows.
	isPG bool
}

// NewQueue returns a Queue bound to db. The caller retains ownership of
// db; Queue does not close it. rebind translates ?-placeholders to the
// driver's native form; pass an identity function (or nil) for SQLite
// and the PostgreSQL dialect's Rebind for pgx.
//
// The Queue detects whether the backend is PostgreSQL by probing rebind:
// if rebind("?") == "$1" the driver is pgx and Claim will use
// FOR UPDATE SKIP LOCKED to prevent concurrent workers from double-claiming.
func NewQueue(db *sql.DB, rebind func(string) string) *Queue {
	if rebind == nil {
		rebind = func(q string) string { return q }
	}
	return &Queue{db: db, rebind: rebind, isPG: rebind("?") == "$1"}
}

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

	// claimSQL selects the candidate rows for the UPDATE. PostgreSQL uses
	// FOR UPDATE SKIP LOCKED so that concurrent workers each see a disjoint
	// slice of available rows; without it two workers can select the same
	// rows in their subquery snapshots and the later UPDATE will simply
	// overwrite the earlier claim token, causing duplicate work.
	// SQLite serializes writers at the file level so no advisory locking is
	// needed there (and it does not support the FOR UPDATE syntax).
	claimSubquery := `
               SELECT generation_id, message_id
                 FROM pending_embeddings
                WHERE generation_id = ?
                  AND claimed_at IS NULL
                ORDER BY message_id
                LIMIT ?`
	if q.isPG {
		claimSubquery += `
                FOR UPDATE SKIP LOCKED`
	}
	claimSQL := `
        UPDATE pending_embeddings
           SET claimed_at = ?, claim_token = ?
         WHERE (generation_id, message_id) IN (` + claimSubquery + `)
        RETURNING message_id`

	ids, err := func() ([]int64, error) {
		rows, err := tx.QueryContext(ctx, q.rebind(claimSQL),
			now, token, int64(gen), batch)
		if err != nil {
			return nil, fmt.Errorf("claim query: %w", err)
		}
		defer func() { _ = rows.Close() }()
		var out []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return nil, fmt.Errorf("scan claimed id: %w", err)
			}
			out = append(out, id)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("claim rows: %w", err)
		}
		return out, nil
	}()
	if err != nil {
		return nil, "", err
	}
	if err := tx.Commit(); err != nil {
		return nil, "", fmt.Errorf("commit claim: %w", err)
	}
	if len(ids) == 0 {
		return nil, "", nil
	}
	// The subquery's ORDER BY decides WHICH rows get claimed, but
	// RETURNING does not guarantee order. Sort explicitly so callers
	// can rely on ascending ids (matters for deterministic test
	// assertions and for pairing ids with fetched message bodies by
	// position).
	slices.Sort(ids)
	return ids, token, nil
}

// completeReleaseChunkRows caps how many message ids go into a single
// Complete/Release statement's IN clause. Each statement binds one
// placeholder per id plus two (generation_id, claim_token), so a chunk
// of 500 ids = 502 bound parameters — comfortably under SQLite's
// 32,766-variable ceiling and PostgreSQL's 65,535 limit even if a
// misconfigured Embeddings.BatchSize claims far more rows than the
// default 32. Mirrors enqueue.go's enqueueChunkRows discipline so a
// single oversized batch never blows the driver bind ceiling. It is a
// var (not const) only so tests can lower it to exercise the chunk
// boundary without driving a multi-thousand-row batch through the DB;
// production never reassigns it.
var completeReleaseChunkRows = 500

// afterChunkHook is a test-only fault-injection seam. When non-nil it is
// invoked after each chunk's statement executes inside execTokenScoped,
// receiving the number of ids processed so far. Returning a non-nil error
// aborts the loop and triggers the transaction rollback, letting tests
// prove cross-chunk atomicity (a failure after an earlier chunk must leave
// zero rows changed). It is always nil in production.
var afterChunkHook func(processed int) error

// Complete deletes the claimed rows from the queue. Only rows whose
// claim_token matches token are removed; any row that was reclaimed or
// re-claimed under a different token is left in place. A nil or empty
// ids slice is a no-op. The ids are processed in chunks (see
// completeReleaseChunkRows); all chunks run inside a single transaction
// (see execTokenScoped), so the delete is atomic across chunks — either
// every matching row is removed or, on any error, none are.
func (q *Queue) Complete(ctx context.Context, gen vector.GenerationID, token string, ids []int64) error {
	const stmt = `
        DELETE FROM pending_embeddings
         WHERE generation_id = ?
           AND claim_token   = ?
           AND message_id IN `
	if err := q.execTokenScoped(ctx, stmt, gen, token, ids); err != nil {
		return fmt.Errorf("delete pending: %w", err)
	}
	return nil
}

// Release returns claimed rows to the pool so another worker can pick
// them up (for embedding failures). Only rows whose claim_token matches
// token are released. A nil or empty ids slice is a no-op. Like
// Complete, the ids are processed in token-scoped chunks (see
// completeReleaseChunkRows) inside a single transaction, so the release
// is atomic across chunks — all matching rows are cleared or, on error,
// none are.
func (q *Queue) Release(ctx context.Context, gen vector.GenerationID, token string, ids []int64) error {
	const stmt = `
        UPDATE pending_embeddings
           SET claimed_at = NULL, claim_token = NULL
         WHERE generation_id = ?
           AND claim_token   = ?
           AND message_id IN `
	if err := q.execTokenScoped(ctx, stmt, gen, token, ids); err != nil {
		return fmt.Errorf("release: %w", err)
	}
	return nil
}

// execTokenScoped runs stmtPrefix (a DELETE or UPDATE ending in
// "... message_id IN ") once per chunk of ids, appending an
// inPlaceholders IN clause and binding (gen, token, ids...) for each
// chunk. Chunking keeps the per-statement bind count under the driver's
// limit regardless of how many ids a single claim produced. Every chunk
// is filtered on generation_id = gen AND claim_token = token, so the
// token-scoped semantics are identical to a single statement; because the
// chunks operate over disjoint id subsets the additive deletes/updates
// compose correctly. A nil or empty ids slice is a no-op.
//
// All chunks run inside a single transaction so the operation is
// all-or-nothing: before chunking, Complete/Release was one atomic
// statement, and wrapping the chunks in a tx restores that guarantee. If
// any chunk fails (DB error or context cancellation) the whole batch is
// rolled back and no rows are left partially deleted/updated while the
// caller still sees an error. Works on both SQLite (mattn supports a tx
// spanning multiple statements) and PostgreSQL (pgx).
func (q *Queue) execTokenScoped(ctx context.Context, stmtPrefix string, gen vector.GenerationID, token string, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}

	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin token-scoped tx: %w", err)
	}
	// Roll back unless Commit below succeeds. After a successful Commit
	// this Rollback is a no-op (sql.ErrTxDone), so it cannot mask success.
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	for start := 0; start < len(ids); start += completeReleaseChunkRows {
		end := min(start+completeReleaseChunkRows, len(ids))
		chunk := ids[start:end]

		args := make([]any, 0, 2+len(chunk))
		args = append(args, int64(gen), token)
		for _, id := range chunk {
			args = append(args, id)
		}
		query := q.rebind(stmtPrefix + inPlaceholders(len(chunk)))
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			// The deferred Rollback discards every earlier chunk in this
			// tx, so the failure leaves the queue untouched. Surface the
			// original error %w-wrapped; the rollback error (if any) is
			// intentionally not propagated so it cannot mask this one.
			return fmt.Errorf("exec chunk: %w", err)
		}
		// Test-only fault-injection seam (nil in production): lets the
		// atomicity tests force a failure AFTER an earlier chunk has
		// already executed inside this tx, exercising the cross-chunk
		// rollback path deterministically. Mirrors the preReturn/OnEmbed
		// seams used elsewhere in this package.
		if afterChunkHook != nil {
			if err := afterChunkHook(end); err != nil {
				return fmt.Errorf("exec chunk: %w", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit token-scoped tx: %w", err)
	}
	committed = true
	return nil
}

// ReclaimStale clears the claim on any pending row whose claimed_at is
// older than olderThan. Returns the number of rows reclaimed.
func (q *Queue) ReclaimStale(ctx context.Context, olderThan time.Duration) (int, error) {
	cutoff := time.Now().Add(-olderThan).Unix()
	res, err := q.db.ExecContext(ctx, q.rebind(`
        UPDATE pending_embeddings
           SET claimed_at = NULL, claim_token = NULL
         WHERE claimed_at IS NOT NULL AND claimed_at < ?`), cutoff)
	if err != nil {
		return 0, fmt.Errorf("reclaim stale: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return int(n), nil
}

// inPlaceholders returns "(?,?,...)" with n placeholders, for building
// IN clauses dynamically. The output uses ? regardless of dialect; the
// caller is expected to run the surrounding query through rebind.
func inPlaceholders(n int) string {
	ph := make([]string, n)
	for i := range ph {
		ph[i] = "?"
	}
	return "(" + strings.Join(ph, ",") + ")"
}

// newToken returns 16 hex characters backed by 8 bytes of crypto/rand.
func newToken() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return hex.EncodeToString(b), nil
}
