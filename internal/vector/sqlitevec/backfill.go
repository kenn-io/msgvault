//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/vector"
)

// embedGenBackfillMigration is the applied_migrations ledger key that
// guards the one-time embed_gen upgrade backfill. Stable string — never
// change it, or the backfill would re-run on every Open.
const embedGenBackfillMigration = "embed_gen_backfill_active_v1"

// backfillStampChunk caps how many message ids go into one stamping
// UPDATE so the bind-parameter count stays well under SQLite's limit.
const backfillStampChunk = 500

// BackfillEmbedGenForUpgrade performs the ONE-TIME upgrade backfill
// (Package A): when an active generation exists, it stamps embed_gen=active
// on every main-DB message that already has >=1 embedding row under that
// generation but whose embed_gen is still NULL.
//
// Why: the embed_gen ADD COLUMN migration does no backfill, so a user
// upgrading from v0.14–v0.15 (who already has an active generation + a
// fully-embedded corpus) would have embed_gen=NULL everywhere. Coverage
// would then report the ENTIRE archive as missing and the worker would
// re-embed all of it. This stamps the already-embedded rows instead — a
// cheap metadata UPDATE, no re-embed.
//
// Guards:
//   - ONE-TIME via the applied_migrations ledger (key
//     embedGenBackfillMigration). Check-then-run-then-mark. It must NOT run
//     on every Open: re-running would clobber repair-encoding's NULL resets
//     before they re-embed, and fight an in-progress rebuild.
//     Accepted residual window: the check and the mark are NOT atomic across
//     PROCESSES, so two concurrent first-opens of a freshly-upgraded DB
//     (before either marks the ledger) can both run the backfill once; the
//     second could re-stamp a row repair-encoding just reset to NULL. This
//     window is ONE-SHOT (only at the first post-upgrade open, before the
//     ledger is marked) and astronomically rare — accepted, not closed
//     (operator decision): the mitigation is operational (run only one
//     embedding process at a time; see README Vector Search). Within a single
//     process the in-tx mark + EmbedJob single-flight lock prevent re-runs.
//   - It lives in the VECTOR layer because the embeddings table is only
//     reachable here (it is in vectors.db on SQLite, a separate *sql.DB
//     from messages).
//   - The stamp UPDATE only touches rows where embed_gen IS NULL, so it
//     never overwrites a row already stamped for another generation.
//
// No-ops cleanly when: no main DB handle, the ledger already records it, no
// active generation, no embeddings table, or no embedded-but-unstamped
// rows. Idempotent.
//
// Cross-DB on SQLite: the embeddings ids come from vectors.db (b.db); the
// stamp and the ledger live in main.db (b.mainDB), two separate *sql.DB
// handles. This mirrors the established cross-DB pattern (see
// EmbeddedMessageCount / dropDeletedFromSource): read ids from vectors.db,
// stamp on main.db.
func (b *Backend) BackfillEmbedGenForUpgrade(ctx context.Context) error {
	if b.mainDB == nil {
		// Management commands may open the backend without the main handle;
		// they never run the backfill.
		return nil
	}
	if b.readOnly {
		// The main handle was opened read-only (MCP: store.OpenReadOnly,
		// _query_only=true). The backfill WRITES messages.embed_gen and the
		// applied_migrations ledger, which the query-only handle rejects.
		// Skip it entirely — mirrors pgvector's SkipMigrate read-only guard.
		// A write-path process (serve, embeddings CLI) runs the backfill
		// instead.
		return nil
	}

	// A main DB without applied_migrations is not a real msgvault store
	// (e.g. a hand-rolled test fixture or a DB opened before the store
	// schema ran); skip the backfill entirely rather than fail Open.
	hasLedger, err := mainTableExists(ctx, b.mainDB, "applied_migrations")
	if err != nil {
		return err
	}
	if !hasLedger {
		return nil
	}

	applied, err := b.backfillApplied(ctx)
	if err != nil {
		return err
	}
	if applied {
		return nil
	}

	// Resolve the active generation. No active generation means nothing to
	// backfill — but we still mark the migration applied so a later
	// just-activated generation does not retroactively trigger a backfill
	// that re-stamps rows repair-encoding may have reset. The active gen at
	// upgrade time is the only one whose pre-existing embeddings predate the
	// embed_gen column; generations created after upgrade are stamped by the
	// worker as it embeds.
	//
	// Intentional scope limit: only the ACTIVE generation is backfilled. Any
	// BUILDING generation that existed pre-upgrade is left unstamped — a
	// resumed rebuild idempotently re-embeds that bounded portion (scan-and-
	// fill skips already-covered rows), so the cost is small and one-time.
	// Per-generation backfill complexity is not worth it for a single-user
	// tool.
	active, err := b.ActiveGeneration(ctx)
	if err != nil {
		if errors.Is(err, vector.ErrNoActiveGeneration) {
			// No work to stamp; a lone ledger INSERT is trivially atomic, so
			// it runs directly on main.db (no transaction needed).
			return b.markBackfillApplied(ctx, b.mainDB)
		}
		return fmt.Errorf("backfill: resolve active generation: %w", err)
	}

	// Distinct message ids that already have an embedding row for the active
	// generation, read from vectors.db. This cross-DB read stays OUTSIDE the
	// main.db transaction below: it is read-only and targets a different
	// *sql.DB handle, so it cannot participate in the main.db tx.
	rows, err := b.db.QueryContext(ctx,
		`SELECT DISTINCT message_id FROM embeddings WHERE generation_id = ?`,
		int64(active.ID))
	if err != nil {
		return fmt.Errorf("backfill: list embedded message ids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("backfill: scan embedded message id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("backfill: iterate embedded message ids: %w", err)
	}

	// Preserve the legacy pending re-embed signal (129 review MEDIUM). Under the
	// OLD design, pending_embeddings was a re-embed flag: a message could carry
	// BOTH an active-gen embedding AND an active-gen pending row (old
	// repair-encoding re-enqueued already-embedded messages; the old worker
	// deleted the pending row only on a successful re-embed). If we stamp
	// embed_gen=active on such a message it reads "covered" forever and is never
	// re-embedded — silent permanent staleness against the corrected text.
	// EXCLUDE every active-gen pending message id from the stamp set so it ends
	// embed_gen=NULL and the scan-and-fill worker re-embeds it. pending_embeddings
	// lives in vectors.db (b.db) on SQLite, the same handle as embeddings.
	pendingIDs, err := b.activeGenPendingIDs(ctx, active.ID)
	if err != nil {
		return err
	}
	if len(pendingIDs) > 0 {
		pendingSet := make(map[int64]struct{}, len(pendingIDs))
		for _, id := range pendingIDs {
			pendingSet[id] = struct{}{}
		}
		kept := ids[:0]
		for _, id := range ids {
			if _, isPending := pendingSet[id]; !isPending {
				kept = append(kept, id)
			}
		}
		ids = kept
	}

	// Atomicity (Codex 129d #2/#3): the embed_gen stamp UPDATE(s) and the
	// ledger mark must be all-or-nothing. messages and applied_migrations
	// both live in main.db, so a single transaction covers every chunk plus
	// the mark. Without it, a crash (or error) after some chunks but before
	// the mark would leave the ledger UNMARKED while embed_gen rows were
	// already stamped → the one-time backfill re-runs on the next Open and
	// clobbers any NULL resets repair-encoding made in the interim. With one
	// tx, a partial-chunk failure rolls back EVERY chunk and the mark, so the
	// migration stays unmarked and the next Open re-runs cleanly from scratch.
	tx, err := b.mainDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("backfill: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Stamp embed_gen=active for those ids on main.db, but only where it is
	// still NULL — never overwrite a row already stamped for a different
	// generation. Chunked to stay under the bind limit; ALL chunks run on the
	// same tx.
	for start := 0; start < len(ids); start += backfillStampChunk {
		end := min(start+backfillStampChunk, len(ids))
		chunk := ids[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, 0, 1+len(chunk))
		args = append(args, int64(active.ID))
		for i, id := range chunk {
			placeholders[i] = "?"
			args = append(args, id)
		}
		q := `UPDATE messages SET embed_gen = ?
		       WHERE embed_gen IS NULL
		         AND id IN (` + strings.Join(placeholders, ",") + `)`
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("backfill: stamp embed_gen: %w", err)
		}
	}

	if err := b.markBackfillApplied(ctx, tx); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("backfill: commit tx: %w", err)
	}
	committed = true
	return nil
}

// activeGenPendingIDs returns the message ids that carry an active-generation
// row in the legacy pending_embeddings table — the OLD "needs (re-)embedding"
// signal. Used by BackfillEmbedGenForUpgrade to EXCLUDE these from the
// embed_gen stamp so they re-embed under scan-and-fill (review MEDIUM).
//
// Returns nil with no error when pending_embeddings does not exist (a fresh
// DB, or one already cleaned up): there is no legacy signal to preserve.
// pending_embeddings lives in vectors.db (b.db).
func (b *Backend) activeGenPendingIDs(ctx context.Context, active vector.GenerationID) ([]int64, error) {
	exists, err := tableExists(ctx, b.db, "pending_embeddings")
	if err != nil {
		return nil, fmt.Errorf("backfill: probe pending_embeddings: %w", err)
	}
	if !exists {
		return nil, nil
	}
	rows, err := b.db.QueryContext(ctx,
		`SELECT DISTINCT message_id FROM pending_embeddings WHERE generation_id = ?`,
		int64(active))
	if err != nil {
		return nil, fmt.Errorf("backfill: list active-gen pending ids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("backfill: scan pending id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("backfill: iterate pending ids: %w", err)
	}
	return ids, nil
}

// dropDeadPendingEmbeddings drops the legacy pending_embeddings queue table
// from vectors.db. The scan-and-fill design replaced the per-generation seed
// queue with a live messages.embed_gen scan, so the table is otherwise unused;
// left in place it only wastes space and confuses operators inspecting
// vectors.db.
//
// It runs on every WRITABLE Open, AFTER BackfillEmbedGenForUpgrade has had a
// chance to consult the table and preserve its re-embed signal (review MEDIUM).
// Doing the drop here rather than in Migrate guarantees the backfill (when it
// runs) sees the table first. It is gated to the writable path so a read-only
// Open leaves the table — and its signal — intact for the next writable open.
// Idempotent: DROP TABLE IF EXISTS is a no-op on fresh DBs and on a second run.
// vectors.db is read-write regardless of the main-handle readOnly flag, so this
// targets b.db unconditionally; the CALLER gates it on the writable Open path.
func (b *Backend) dropDeadPendingEmbeddings(ctx context.Context) error {
	if _, err := b.db.ExecContext(ctx, `DROP TABLE IF EXISTS pending_embeddings`); err != nil {
		return fmt.Errorf("drop dead pending_embeddings table: %w", err)
	}
	return nil
}

// resetOrphanedEmbedGen clears messages.embed_gen for every main-DB message
// whose stamp references a generation id that does NOT exist in
// index_generations (an "orphaned" stamp). It runs on every WRITABLE Open
// BEFORE BackfillEmbedGenForUpgrade.
//
// Why: index_generations.id AUTOINCREMENTs inside the REPLACEABLE vectors.db,
// while embed_gen stamps live in the durable main.db. If a user deletes and
// recreates vectors.db but keeps main.db, the fresh index_generations restarts
// ids at 1 while main.db still carries old stamps (e.g. embed_gen=1). A later
// rebuild then reuses gen id 1, the coverage scan predicate
// (embed_gen IS NULL OR embed_gen <> target) treats those stale stamps as
// already-covered, coverage reaches missing==0, and an EMPTY index is
// activated — search returns nothing while coverage claims done. Clearing
// orphaned stamps before any rebuild can reuse id 1 closes that hole.
//
// This is the precise, false-positive-proof form of the operator's
// "recreate-detection reset": a stamp pointing to a STILL-EXISTING generation
// row (active, building, OR retired — retire only flips state, it does not
// delete the row) is KEPT. So the normal activate/retire flow, where a rebuild
// re-stamps live messages to the new active gen and the old gen's row is merely
// marked retired, never trips this reset. Only a genuinely vanished gen id
// (vectors.db recreated/wiped) triggers a clear.
//
// Cross-DB on SQLite: the valid gen-id set comes from vectors.db (b.db); the
// reset runs against main.db (b.mainDB). When the valid set is EMPTY (recreated
// vectors.db), the predicate degrades to "all non-NULL stamps" — handled
// explicitly because `NOT IN ()` is a SQL pitfall. The valid set is tiny (a
// handful of generations), so a literal IN-list is fine.
//
// Guards (mirror BackfillEmbedGenForUpgrade): no-op when the main handle is
// absent (management commands) or read-only (MCP). NOT ledger-guarded: a
// recreate can happen between any two process starts, so this must re-check
// every writable Open. It is cheap and idempotent — a second run finds no
// orphans and updates nothing.
func (b *Backend) resetOrphanedEmbedGen(ctx context.Context) error {
	if b.mainDB == nil {
		// Management commands open the backend without the main handle.
		return nil
	}
	if b.readOnly {
		// Read-only main handle (MCP): the reset WRITES messages.embed_gen,
		// which the query-only handle rejects. Skip — a write-path process
		// (serve, embeddings CLI) performs the reset instead. Mirrors the
		// backfill's b.readOnly guard.
		return nil
	}

	// A main DB without applied_migrations is not a real msgvault store (e.g.
	// a hand-rolled test fixture, or a DB opened before the store schema ran);
	// such a fixture also lacks the embed_gen column. Skip the reset entirely
	// rather than fail Open — mirrors BackfillEmbedGenForUpgrade's identical
	// guard so the two open-time steps gate the same way.
	hasLedger, err := mainTableExists(ctx, b.mainDB, "applied_migrations")
	if err != nil {
		return err
	}
	if !hasLedger {
		return nil
	}

	// Collect the set of valid generation ids from vectors.db.
	rows, err := b.db.QueryContext(ctx, `SELECT id FROM index_generations`)
	if err != nil {
		return fmt.Errorf("reset orphaned embed_gen: list generation ids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var validIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("reset orphaned embed_gen: scan generation id: %w", err)
		}
		validIDs = append(validIDs, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reset orphaned embed_gen: iterate generation ids: %w", err)
	}

	// Empty valid set (recreated/empty vectors.db): every non-NULL stamp is
	// orphaned. `NOT IN ()` is a SQL pitfall, so special-case it to a plain
	// "clear all non-NULL stamps" UPDATE.
	if len(validIDs) == 0 {
		if _, err := b.mainDB.ExecContext(ctx,
			`UPDATE messages SET embed_gen = NULL WHERE embed_gen IS NOT NULL`); err != nil {
			return fmt.Errorf("reset orphaned embed_gen: clear all stamps: %w", err)
		}
		return nil
	}

	// Non-empty valid set: clear only stamps that fall outside it. The set is
	// tiny, so a literal IN-list is well under SQLite's bind limit.
	placeholders := make([]string, len(validIDs))
	args := make([]any, len(validIDs))
	for i, id := range validIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	q := `UPDATE messages SET embed_gen = NULL
	       WHERE embed_gen IS NOT NULL
	         AND embed_gen NOT IN (` + strings.Join(placeholders, ",") + `)`
	if _, err := b.mainDB.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("reset orphaned embed_gen: clear orphaned stamps: %w", err)
	}
	return nil
}

// backfillApplied reports whether the one-time backfill ledger row exists
// in main.db. A missing applied_migrations table (older main schema) is
// treated as "not applied" — the table is created by the store schema, so
// this only matters in unusual test setups; the markBackfillApplied write
// would then surface the real error.
func (b *Backend) backfillApplied(ctx context.Context) (bool, error) {
	var n int
	if err := b.mainDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM applied_migrations WHERE name = ?`,
		embedGenBackfillMigration).Scan(&n); err != nil {
		return false, fmt.Errorf("backfill: check ledger: %w", err)
	}
	return n > 0, nil
}

// execer is the subset of *sql.DB / *sql.Tx the ledger mark needs, so
// markBackfillApplied can run either directly on main.db (lone INSERT, the
// no-active-gen path) or inside the backfill transaction (alongside the
// chunked embed_gen UPDATEs, for atomicity).
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// markBackfillApplied records the one-time backfill in main.db's ledger via
// the given execer. INSERT OR IGNORE keeps it idempotent under a concurrent
// Open. Pass b.mainDB for a standalone mark, or the backfill tx so the mark
// commits atomically with the embed_gen UPDATEs.
func (b *Backend) markBackfillApplied(ctx context.Context, ex execer) error {
	if _, err := ex.ExecContext(ctx,
		`INSERT OR IGNORE INTO applied_migrations (name) VALUES (?)`,
		embedGenBackfillMigration); err != nil {
		return fmt.Errorf("backfill: mark ledger: %w", err)
	}
	return nil
}

// mainTableExists asks sqlite_master in db (the MAIN db, distinct from
// vectors.db) whether a regular or virtual table named `name` exists.
func mainTableExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type IN ('table','virtual') AND name = ?`,
		name).Scan(&n); err != nil {
		return false, fmt.Errorf("backfill: probe %s: %w", name, err)
	}
	return n > 0, nil
}
