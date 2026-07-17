//go:build pgvector

package pgvector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/vector"
)

// embedGenBackfillMigration is the applied_migrations ledger key that
// guards the one-time embed_gen upgrade backfill. Stable string — never
// change it, or the backfill would re-run on every Open.
const embedGenBackfillMigration = "embed_gen_backfill_active_v1"

// BackfillEmbedGenForUpgrade performs the ONE-TIME upgrade backfill
// (Package A): when an active generation exists, it stamps embed_gen=active
// on every message that already has >=1 embedding row under that generation
// but whose embed_gen is still NULL.
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
//     reachable here.
//   - The stamp UPDATE only touches rows where embed_gen IS NULL, so it
//     never overwrites a row already stamped for another generation.
//
// No-ops cleanly when the ledger already records it, there is no active
// generation, the embeddings table is empty, or there are no
// embedded-but-unstamped rows. Idempotent.
//
// Single DB on PostgreSQL: messages, embeddings, and the ledger all share
// b.db, so the backfill is one EXISTS-correlated UPDATE.
func (b *Backend) BackfillEmbedGenForUpgrade(ctx context.Context) error {
	// A database without applied_migrations is not a real msgvault store
	// (e.g. a minimal test fixture, or a DB opened before the store schema
	// ran); skip the backfill entirely rather than fail Open.
	var ledger *string
	if err := b.db.QueryRowContext(ctx,
		`SELECT to_regclass('applied_migrations')::text`).Scan(&ledger); err != nil {
		return fmt.Errorf("backfill: probe ledger: %w", err)
	}
	if ledger == nil {
		return nil
	}

	// Robustness guard (mirrors resetOrphanedEmbedGen and the SQLite side):
	// the stamp UPDATE writes messages.embed_gen, so a DB whose messages table
	// predates the embed_gen column (a partial restore, or a writable Open that
	// ran before store.InitSchema added the column) must skip rather than fail
	// Open. The column is added by PostgreSQLDialect.LegacyColumnMigrations and
	// is present on any store created via the full schema.
	hasCol, err := messagesHasEmbedGen(ctx, b.db)
	if err != nil {
		return err
	}
	if !hasCol {
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
	// that re-stamps rows repair-encoding may have reset. A lone ledger
	// INSERT is trivially atomic, so it runs directly (no transaction).
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
			return b.markBackfillAppliedExec(ctx, b.db)
		}
		return fmt.Errorf("backfill: resolve active generation: %w", err)
	}

	// Atomicity (Codex 129d #2/#3): the embed_gen stamp UPDATE and the
	// ledger mark must be all-or-nothing. messages and applied_migrations
	// share b.db, so a single transaction covers both. If the process
	// crashes (or any error occurs) after the UPDATE but before the mark, an
	// autocommit pair would leave the ledger UNMARKED while embed_gen was
	// already stamped → the supposedly one-time backfill re-runs on the next
	// Open and clobbers any NULL resets repair-encoding made in the interim.
	// Wrapping both in one tx makes a crash leave the DB exactly pre-backfill
	// (no stamps, no mark), so the next Open re-runs cleanly.
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("backfill: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Disable the pool-wide 30s statement_timeout for this tx: on a real corpus
	// the stamp UPDATE below is a ≈28s nested-loop EXISTS-correlated semi-join
	// over the full messages table (≈53k rows), which — combined with the
	// preceding resetOrphanedEmbedGen UPDATE on the same Open — exceeds the
	// shared store pool's statement_timeout=30s, cancelling the one-time
	// upgrade backfill at 30s (SQLSTATE 57014) and rolling it back so the
	// upgrade never completes (finding S1 family, mirroring
	// Migrate/ActivateGeneration/RetireGeneration/EnsureVectorIndex). SET LOCAL
	// is tx-scoped and auto-resets on commit/rollback, so the disabled timeout
	// cannot leak onto other pooled connections. Must be the first statement in
	// the tx to cover the stamp UPDATE.
	if _, err := tx.ExecContext(ctx, "SET LOCAL statement_timeout = 0"); err != nil {
		return fmt.Errorf("backfill: disable statement_timeout: %w", err)
	}

	// Preserve the legacy pending re-embed signal. Under the
	// OLD design, pending_embeddings was a re-embed flag: a message could carry
	// BOTH an active-gen embedding AND an active-gen pending row (old
	// repair-encoding re-enqueued already-embedded messages; the old worker
	// deleted the pending row only on a successful re-embed). Stamping
	// embed_gen=active on such a message would read "covered" forever and never
	// re-embed it — silent permanent staleness. EXCLUDE active-gen pending ids
	// from the stamp so they end embed_gen=NULL and scan-and-fill re-embeds them.
	//
	// pending_embeddings may already be gone (a DB upgraded before this change
	// dropped it in Migrate, or a fresh DB never had it). Probe first and add the
	// NOT IN exclusion only when the table is present; otherwise the plain stamp
	// (no legacy signal to preserve) runs.
	pendingExists, err := pendingEmbeddingsExists(ctx, tx)
	if err != nil {
		return err
	}
	stamp := `UPDATE messages SET embed_gen = $1
		  WHERE embed_gen IS NULL
		    AND EXISTS (
		        SELECT 1 FROM embeddings e
		         WHERE e.message_id = messages.id
		           AND e.generation_id = $1)`
	if pendingExists {
		stamp += `
		    AND messages.id NOT IN (
		        SELECT message_id FROM pending_embeddings WHERE generation_id = $1)`
	}
	// Stamp embed_gen=active for messages with an embedding row under the
	// active generation, only where embed_gen is still NULL (never overwrite
	// a row stamped for another generation), excluding active-gen pending ids.
	if _, err := tx.ExecContext(ctx, stamp, int64(active.ID)); err != nil {
		return fmt.Errorf("backfill: stamp embed_gen: %w", err)
	}

	if err := b.markBackfillAppliedExec(ctx, tx); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("backfill: commit tx: %w", err)
	}
	committed = true
	return nil
}

// rowQueryer is the subset of *sql.DB / *sql.Tx that pendingEmbeddingsExists
// needs, so it can probe either on the pool or inside the backfill tx.
type rowQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// pendingEmbeddingsExists reports whether the legacy pending_embeddings table
// is present. Used by BackfillEmbedGenForUpgrade to decide whether to apply the
// active-gen pending exclusion. to_regclass returns NULL for a
// non-existent relation, so a NULL scan means "absent".
func pendingEmbeddingsExists(ctx context.Context, q rowQueryer) (bool, error) {
	var reg *string
	if err := q.QueryRowContext(ctx,
		`SELECT to_regclass('pending_embeddings')::text`).Scan(&reg); err != nil {
		return false, fmt.Errorf("backfill: probe pending_embeddings: %w", err)
	}
	return reg != nil, nil
}

// dropDeadPendingEmbeddings drops the legacy pending_embeddings queue table.
// The scan-and-fill design replaced the per-generation seed queue with a live
// messages.embed_gen scan, so the table is otherwise unused; left in place it
// only wastes space and confuses operators.
//
// It runs on every WRITABLE Open, AFTER BackfillEmbedGenForUpgrade has had a
// chance to consult the table and preserve its re-embed signal.
// Doing the drop here rather than in Migrate guarantees the backfill (when it
// runs) sees the table first; gated to the writable Open path so a read-only
// Open leaves the table — and its signal — intact for the next writable open.
// Runs inside a tx that lifts the pool-wide statement_timeout (DROP takes an
// ACCESS EXCLUSIVE lock; the lock-wait alone can exceed 30s on a busy serve),
// matching the sibling write helpers. Idempotent: DROP TABLE IF EXISTS is a
// no-op on fresh DBs and on a second run.
func (b *Backend) dropDeadPendingEmbeddings(ctx context.Context) error {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("drop dead pending_embeddings: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, "SET LOCAL statement_timeout = 0"); err != nil {
		return fmt.Errorf("drop dead pending_embeddings: disable statement_timeout: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS pending_embeddings`); err != nil {
		return fmt.Errorf("drop dead pending_embeddings table: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("drop dead pending_embeddings: commit tx: %w", err)
	}
	return nil
}

// resetOrphanedEmbedGen clears messages.embed_gen for every message whose
// stamp references a generation id that does NOT exist in index_generations
// (an "orphaned" stamp). It runs on every WRITABLE Open BEFORE
// BackfillEmbedGenForUpgrade.
//
// Why: this mirrors the sqlitevec safety net. On SQLite, index_generations
// lives in a replaceable vectors.db and embed_gen lives in the durable
// main.db, so a vectors.db wipe restarts gen ids at 1 while old stamps linger,
// masking coverage and activating an empty index. On PG everything shares one
// database, so a true recreate drops messages and index_generations together —
// the orphan window only opens under a partial restore (e.g. messages restored
// but embeddings/generations not). The reset is kept for symmetry and to
// defend that partial-restore case.
//
// False-positive-proof: a stamp pointing to a still-existing generation row
// (active, building, OR retired — retire only flips state, it does not delete
// the row) is KEPT, so the normal activate/retire flow never trips this reset.
// Only a vanished gen id triggers a clear.
//
// Single DB on PG: one UPDATE with a NOT IN subquery. `NOT IN (subquery)`
// handles the empty case correctly in PostgreSQL (it degrades to "all rows"),
// and index_generations.id is NOT NULL so the NULL-in-subquery pitfall cannot
// arise.
//
// Guards (mirror BackfillEmbedGenForUpgrade / the Open ReadOnly gate): the
// caller (Open) skips this on the ReadOnly path. NOT ledger-guarded: it
// re-checks every writable Open; cheap + idempotent (a second run finds no
// orphans and updates nothing).
func (b *Backend) resetOrphanedEmbedGen(ctx context.Context) error {
	// Robustness guard (mirrors the SQLite resetOrphanedEmbedGen, which skips
	// when applied_migrations is absent because "such a fixture also lacks the
	// embed_gen column"): the reset UPDATE writes messages.embed_gen, so a DB
	// whose messages table predates the embed_gen column must skip rather than
	// fail Open with `column "embed_gen" does not exist (SQLSTATE 42703)`. This
	// happens on a partial restore, or when a writable Open (e.g. `msgvault
	// search --vector` via search_vector.go) runs before store.InitSchema has
	// added the column. The column is added by
	// PostgreSQLDialect.LegacyColumnMigrations and is present on any store
	// created via the full schema.
	hasCol, err := messagesHasEmbedGen(ctx, b.db)
	if err != nil {
		return err
	}
	if !hasCol {
		return nil
	}

	// Run the UPDATE inside a short tx that disables the pool-wide 30s
	// statement_timeout (finding S1 family, mirroring
	// Migrate/ActivateGeneration/RetireGeneration/EnsureVectorIndex and the
	// upgrade backfill below). This reset is normally cheap (0 rows when there
	// are no orphans), but on a partial restore it can clear corpus-size stamps
	// and — running right before BackfillEmbedGenForUpgrade on the same Open —
	// would otherwise risk a 30s cancellation (SQLSTATE 57014). SET LOCAL is
	// tx-scoped and auto-resets on commit/rollback, so the disabled timeout
	// cannot leak onto other pooled connections. Must be the first statement in
	// the tx. Behaviour is otherwise identical: same WHERE clause, idempotent
	// (a second run finds no orphans), and gated by the ReadOnly Open path.
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("reset orphaned embed_gen: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, "SET LOCAL statement_timeout = 0"); err != nil {
		return fmt.Errorf("reset orphaned embed_gen: disable statement_timeout: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE messages SET embed_gen = NULL
		  WHERE embed_gen IS NOT NULL
		    AND embed_gen NOT IN (SELECT id FROM index_generations)`); err != nil {
		return fmt.Errorf("reset orphaned embed_gen: clear orphaned stamps: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("reset orphaned embed_gen: commit tx: %w", err)
	}
	return nil
}

// messagesHasEmbedGen reports whether the messages table has an embed_gen
// column. Used by resetOrphanedEmbedGen and BackfillEmbedGenForUpgrade to
// no-op rather than fail Open when the column is absent (a DB whose messages
// predates the column, or a writable Open before store.InitSchema ran). A
// missing messages table also reports false. Mirrors the SQLite side's
// table-existence guard.
func messagesHasEmbedGen(ctx context.Context, db *sql.DB) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.columns
		  WHERE table_name = 'messages' AND column_name = 'embed_gen'
		    AND table_schema = ANY (current_schemas(false))`).Scan(&n); err != nil {
		return false, fmt.Errorf("probe messages.embed_gen column: %w", err)
	}
	return n > 0, nil
}

// backfillApplied reports whether the one-time backfill ledger row exists.
func (b *Backend) backfillApplied(ctx context.Context) (bool, error) {
	var n int
	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM applied_migrations WHERE name = $1`,
		embedGenBackfillMigration).Scan(&n); err != nil {
		return false, fmt.Errorf("backfill: check ledger: %w", err)
	}
	return n > 0, nil
}

// execer is the subset of *sql.DB / *sql.Tx the ledger mark needs, so
// markBackfillAppliedExec can run either directly (lone INSERT, the
// no-active-gen path) or inside the backfill transaction (alongside the
// embed_gen UPDATE, for atomicity).
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// markBackfillAppliedExec records the one-time backfill in the ledger via
// the given execer. ON CONFLICT DO NOTHING keeps it idempotent under a
// concurrent Open. Pass b.db for a standalone mark, or the backfill tx so
// the mark commits atomically with the embed_gen UPDATE.
func (b *Backend) markBackfillAppliedExec(ctx context.Context, ex execer) error {
	if _, err := ex.ExecContext(ctx,
		`INSERT INTO applied_migrations (name) VALUES ($1) ON CONFLICT DO NOTHING`,
		embedGenBackfillMigration); err != nil {
		return fmt.Errorf("backfill: mark ledger: %w", err)
	}
	return nil
}
