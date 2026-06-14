package embed

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/sync"
	"go.kenn.io/msgvault/internal/vector"
)

// afterGenSnapshotHook is a test-only synchronization seam. When non-nil
// it is invoked once inside EnqueueMessages' transaction AFTER the
// non-retired generation snapshot is read but BEFORE any per-generation
// re-validation or pending insert runs. It lets the concurrency
// regression test commit a RetireGeneration at exactly the window the
// orphan-pending race opens (snapshot read → retire commits → enqueue
// inserts), proving the locked re-validation excludes the now-retired
// generation. It is always nil in production. Mirrors the afterChunkHook
// seam in queue.go.
var afterGenSnapshotHook func()

// enqueueChunkRows caps how many (gen, message) tuples go into a single
// INSERT statement. Each row binds 3 placeholders (generation_id,
// message_id, enqueued_at), so 500 rows = 1,500 bound parameters. The
// compiled SQLite driver (mattn/go-sqlite3) allows up to 32,766 bound
// variables per statement, so 1,500 is comfortably within budget; the
// value is also small enough to avoid an oversized prepared statement on
// PostgreSQL. (For reference, the store package caps multi-row inserts at
// 900 params to stay under SQLite's historical 999 limit — see
// insertInChunks.)
//
// The Enqueuer can be handed up to ~5,000 IDs by sync, fanned out across
// up to two non-retired generations; without chunking that would be
// 3×5,000 = 15,000 placeholders per statement, which bloats the prepared
// statement. 500 keeps every statement comfortably small on both SQLite
// and PostgreSQL while still amortizing the per-statement overhead (a
// 5,000-ID batch becomes 10 statements, not 5,000 single-row inserts).
const enqueueChunkRows = 500

// Compile-time assertion that *Enqueuer satisfies the sync.EmbedEnqueuer
// interface expected by internal/sync.Syncer.
var _ sync.EmbedEnqueuer = (*Enqueuer)(nil)

// Enqueuer inserts message IDs into pending_embeddings for every
// non-retired generation. Implements the EmbedEnqueuer interface
// expected by internal/sync.
//
// Dual-enqueue is intentional: when a rebuild is in progress there are
// two non-retired generations (active + building); every newly-synced
// message gets queued into both so the building index stays current.
type Enqueuer struct {
	db *sql.DB
	// rebind translates ?-placeholders to the driver's native form; nil
	// is normalized to identity (SQLite). insertOrIgnore rewrites a
	// complete "INSERT OR IGNORE INTO ..." statement into the dialect's
	// conflict-ignoring form (SQLite: identity; PostgreSQL: strips
	// "OR IGNORE" and appends "ON CONFLICT DO NOTHING"); nil is
	// normalized to identity. Both are applied in the same order the
	// store package uses: insertOrIgnore first (it operates on the
	// ?-placeholder SQLite form), then rebind.
	rebind         func(string) string
	insertOrIgnore func(string) string
	// isPG is true when the underlying driver is PostgreSQL. When set,
	// EnqueueMessages re-validates each generation under a row lock
	// (SELECT ... FOR NO KEY UPDATE) that conflicts with the implicit
	// no-key tuple lock RetireGeneration/ActivateGeneration's
	// state-flip UPDATE takes, so a generation retired concurrently with
	// an enqueue cannot end up with an orphan pending row. SQLite does not
	// support the FOR NO KEY UPDATE syntax (and does not need it — its
	// file-level write serialization plus busy_timeout already serialize
	// the enqueue against the retire), so the clause is omitted there.
	isPG bool
}

// NewEnqueuer returns an Enqueuer backed by the embeddings database
// (vectors.db on SQLite, the shared main DB on PostgreSQL). rebind and
// insertOrIgnore make the Enqueuer dialect-portable without importing
// internal/store, mirroring NewQueue's decoupled func style: pass nil
// for both on SQLite (identity), or the dialect's Rebind and
// InsertOrIgnore for pgx.
//
// Like NewQueue, the Enqueuer detects PostgreSQL by probing rebind: if
// rebind("?") == "$1" the driver is pgx and the per-generation
// re-validation SELECT acquires a FOR NO KEY UPDATE row lock.
func NewEnqueuer(db *sql.DB, rebind, insertOrIgnore func(string) string) *Enqueuer {
	if rebind == nil {
		rebind = func(q string) string { return q }
	}
	if insertOrIgnore == nil {
		insertOrIgnore = func(q string) string { return q }
	}
	return &Enqueuer{
		db:             db,
		rebind:         rebind,
		insertOrIgnore: insertOrIgnore,
		isPG:           rebind("?") == "$1",
	}
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

	gens, err := func() ([]int64, error) {
		rows, err := tx.QueryContext(ctx,
			e.rebind(`SELECT id FROM index_generations WHERE state != ?`),
			string(vector.GenerationRetired))
		if err != nil {
			return nil, fmt.Errorf("select non-retired generations: %w", err)
		}
		defer func() { _ = rows.Close() }()
		var out []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return nil, fmt.Errorf("scan generation id: %w", err)
			}
			out = append(out, id)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("iterate generations: %w", err)
		}
		return out, nil
	}()
	if err != nil {
		return err
	}
	if len(gens) == 0 {
		return tx.Commit()
	}

	// Test-only synchronization seam (nil in production): fires after the
	// non-retired snapshot is read but before the locked re-validation +
	// inserts below, so the concurrency regression test can commit a
	// RetireGeneration inside the exact window the orphan-pending race opens.
	if afterGenSnapshotHook != nil {
		afterGenSnapshotHook()
	}

	// revalidate re-reads a generation's state under a row lock that
	// conflicts with the no-key tuple lock RetireGeneration /
	// ActivateGeneration's state-flip UPDATE takes, then reports whether
	// the locked re-read still sees the generation as non-retired. The
	// initial non-retired snapshot above is read at the start of this tx
	// under READ COMMITTED, so without this guard a concurrent retire could
	// commit between that snapshot and the INSERT below — the FK insert
	// takes only FOR KEY SHARE on index_generations, which does NOT conflict
	// with retire's FOR NO KEY UPDATE, so the orphan pending row would commit
	// and never be reaped (pickTarget skips retired gens; the retired
	// index_generations row is preserved so its ON DELETE CASCADE never
	// fires). Re-validating under FOR NO KEY UPDATE serializes the two
	// interleavings:
	//   - enqueue-first: retire's state-flip UPDATE blocks on this lock,
	//     then its DELETE removes the rows we just inserted -> no orphan.
	//   - retire-first: this locking SELECT blocks until retire commits, then
	//     re-reads state='retired' and returns false -> we insert nothing.
	// On SQLite the FOR NO KEY UPDATE clause is omitted (unsupported syntax);
	// its file-level write serialization + busy_timeout force a retry on the
	// losing writer, so the same invariant holds without an explicit lock.
	revalidate := `SELECT id FROM index_generations WHERE id = ? AND state != ?`
	if e.isPG {
		revalidate += ` FOR NO KEY UPDATE`
	}
	revalidate = e.rebind(revalidate)

	now := time.Now().Unix()
	for _, g := range gens {
		// Re-read the generation's state under a row lock; skip it if it has
		// been retired since the non-retired snapshot above. Reuses the same
		// tx so the lock is held through the INSERTs below.
		var lockedID int64
		err := tx.QueryRowContext(ctx, revalidate, g, string(vector.GenerationRetired)).Scan(&lockedID)
		if errors.Is(err, sql.ErrNoRows) {
			// Retired concurrently (PG: by a now-committed retire we just
			// blocked on; SQLite: by a retire that won the write race) — do
			// not enqueue, leaving no orphan pending row for this gen.
			continue
		}
		if err != nil {
			return fmt.Errorf("re-validate generation %d: %w", g, err)
		}
		// Bulk-insert one row per (gen, message) pair via chunked multi-row
		// VALUES statements. Each (gen, message) tuple binds 3 parameters, so
		// we cap each statement at enqueueChunkRows rows to stay under
		// SQLite's parameter limit and avoid an oversized prepared statement
		// on either backend. For a 5,000-message batch with two non-retired
		// generations this is ~20 writes against the embeddings DB lock
		// instead of 10,000 single-row inserts — keeps the embed worker's
		// Claim from starving while sync flushes. The previous json_each
		// path issued one statement per generation but is SQLite-only;
		// chunked VALUES is portable to pgx.
		for start := 0; start < len(messageIDs); start += enqueueChunkRows {
			end := min(start+enqueueChunkRows, len(messageIDs))
			chunk := messageIDs[start:end]

			placeholders := make([]string, len(chunk))
			args := make([]any, 0, len(chunk)*3)
			for i, id := range chunk {
				placeholders[i] = "(?, ?, ?)"
				args = append(args, g, id, now)
			}
			// Build the SQLite-form statement, then apply the dialect's
			// insert-or-ignore rewrite (operates on ? placeholders),
			// then rebind ? → $N. Same ordering as the store package's
			// InsertOrIgnore-then-loggedDB-Rebind pipeline.
			stmt := `INSERT OR IGNORE INTO pending_embeddings (generation_id, message_id, enqueued_at) VALUES ` +
				strings.Join(placeholders, ",")
			stmt = e.rebind(e.insertOrIgnore(stmt))
			if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
				return fmt.Errorf("insert pending (gen=%d): %w", g, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit enqueue: %w", err)
	}
	return nil
}
