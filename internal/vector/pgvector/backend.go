//go:build pgvector

// Package pgvector implements vector.Backend using the pgvector
// PostgreSQL extension, co-located with the main pgx-backed connection.
// Embeddings are stored in the same database as messages — there is no
// separate vectors.db. Build with `-tags pgvector` to enable.
package pgvector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

// Compile-time check that *Backend satisfies vector.Backend.
var _ vector.Backend = (*Backend)(nil)

// annOverFetchFactor multiplies k for the inner ANN scan so that after
// GROUP BY dedup across multi-chunk messages, at least k distinct
// messages survive with high probability.
const annOverFetchFactor = 4

// Options configures Open. The same *sql.DB handle backs both the
// embedding schema and the main msgvault schema; pgvector embeddings
// live in the same Postgres database.
type Options struct {
	// DB is the pgx-backed handle to the database that contains both
	// the msgvault main schema and the pgvector embedding tables.
	DB *sql.DB
	// Dimension is the default dimension used to eagerly create the
	// per-dimension HNSW index on first migration. Optional; if zero
	// the index is created on first CreateGeneration.
	Dimension int
	// SkipMigrate suppresses the automatic schema migration on Open.
	// Set this when the caller holds a read-only connection (e.g. the
	// MCP server), where CREATE EXTENSION and DDL statements are
	// rejected by PostgreSQL with SQLSTATE 25006.
	SkipMigrate bool
	// SkipExtension suppresses only the `CREATE EXTENSION IF NOT EXISTS
	// vector` step during migration while still creating the schema
	// tables and indexes. Set this when the vector extension is
	// managed/installed externally (e.g. a DBA pre-installs it on a
	// locked-down or managed PostgreSQL where CREATE EXTENSION requires
	// superuser). Unlike SkipMigrate (which suppresses ALL DDL for the
	// read-only path), SkipExtension still runs schema + index creation
	// so a non-superuser holding DDL rights can bring the embedding
	// tables up. Ignored when SkipMigrate is true.
	SkipExtension bool
}

// Backend implements vector.Backend against a PostgreSQL database
// with the pgvector extension. The same *sql.DB also serves the main
// msgvault schema (messages, message_recipients, message_labels).
type Backend struct {
	db *sql.DB
}

// Open verifies the database is reachable, applies the embedding schema
// (creating the vector extension if necessary), and returns a Backend.
// The DB handle is shared with the main msgvault store; callers retain
// ownership and Close() is a no-op for the handle itself.
func Open(ctx context.Context, opts Options) (*Backend, error) {
	if opts.DB == nil {
		return nil, errors.New("pgvector.Open: Options.DB is required")
	}
	if !opts.SkipMigrate {
		if err := Migrate(ctx, opts.DB, opts.Dimension, opts.SkipExtension); err != nil {
			return nil, fmt.Errorf("pgvector migrate: %w", err)
		}
	}
	return &Backend{db: opts.DB}, nil
}

// Close is a no-op for the pgvector backend: the *sql.DB handle is
// owned by the main store and closed there. Provided to satisfy the
// vector.Backend interface.
func (b *Backend) Close() error { return nil }

// DB returns the underlying *sql.DB. Exposed to mirror the sqlitevec
// surface; callers that need the shared pool (e.g. the embed worker)
// can retrieve it here instead of carrying the main handle separately.
func (b *Backend) DB() *sql.DB { return b.db }

// CreateGeneration allocates a new building generation and seeds
// pending_embeddings with every currently-embeddable message in
// messages. Mirrors the sqlitevec semantics (§5.1): if a building row
// with the same fingerprint already exists, returns its id so a crashed
// rebuild can resume; a mismatched fingerprint surfaces
// vector.ErrBuildingInProgress.
func (b *Backend) CreateGeneration(ctx context.Context, model string, dim int, fingerprint string) (vector.GenerationID, error) {
	if err := EnsureVectorIndex(ctx, b.db, dim); err != nil {
		return 0, err
	}
	fp := fingerprint
	if fp == "" {
		fp = fmt.Sprintf("%s:%d", model, dim)
	}
	now := time.Now().Unix()

	gen, isNew, err := b.claimOrInsertBuilding(ctx, model, dim, fp, now)
	if err != nil {
		return 0, err
	}

	if !isNew {
		seeded, err := b.isGenerationSeeded(ctx, gen)
		if err != nil {
			return 0, err
		}
		if seeded {
			return gen, nil
		}
	}
	if err := b.seedPending(ctx, gen, now); err != nil {
		return 0, err
	}
	if err := b.markGenerationSeeded(ctx, gen, now); err != nil {
		return 0, err
	}
	return gen, nil
}

func (b *Backend) isGenerationSeeded(ctx context.Context, gen vector.GenerationID) (bool, error) {
	var seededAt sql.NullInt64
	err := b.db.QueryRowContext(ctx,
		`SELECT seeded_at FROM index_generations WHERE id = $1`, int64(gen)).Scan(&seededAt)
	if err != nil {
		return false, fmt.Errorf("read seeded_at: %w", err)
	}
	return seededAt.Valid, nil
}

func (b *Backend) markGenerationSeeded(ctx context.Context, gen vector.GenerationID, now int64) error {
	if _, err := b.db.ExecContext(ctx,
		`UPDATE index_generations SET seeded_at = COALESCE(seeded_at, $1) WHERE id = $2`,
		now, int64(gen)); err != nil {
		return fmt.Errorf("mark generation seeded: %w", err)
	}
	return nil
}

// EnsureSeeded mirrors sqlitevec.EnsureSeeded: re-runs the initial seed
// pass when seeded_at is NULL so an interrupted resume cannot activate
// an empty generation.
func (b *Backend) EnsureSeeded(ctx context.Context, gen vector.GenerationID) error {
	var state string
	err := b.db.QueryRowContext(ctx,
		`SELECT state FROM index_generations WHERE id = $1`, int64(gen)).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %d", vector.ErrUnknownGeneration, gen)
	}
	if err != nil {
		return fmt.Errorf("lookup generation %d: %w", gen, err)
	}
	if state != string(vector.GenerationBuilding) {
		return fmt.Errorf("%w: generation %d state=%q", vector.ErrGenerationNotBuilding, gen, state)
	}
	seeded, err := b.isGenerationSeeded(ctx, gen)
	if err != nil {
		return err
	}
	if seeded {
		return nil
	}
	now := time.Now().Unix()
	if err := b.seedPending(ctx, gen, now); err != nil {
		return err
	}
	return b.markGenerationSeeded(ctx, gen, now)
}

// claimOrInsertBuilding returns (id, isNew, err). See sqlitevec for
// rationale — same race-recovery shape, translated to pgx error codes.
func (b *Backend) claimOrInsertBuilding(ctx context.Context, model string, dim int, fp string, now int64) (vector.GenerationID, bool, error) {
	if id, existingFP, ok, err := b.lookupBuilding(ctx); err != nil {
		return 0, false, err
	} else if ok {
		if existingFP != fp {
			return 0, false, fmt.Errorf("%w: existing building fingerprint=%q, requested=%q — activate or retire it before starting a new rebuild",
				vector.ErrBuildingInProgress, existingFP, fp)
		}
		return id, false, nil
	}

	var newID int64
	err := b.db.QueryRowContext(ctx,
		`INSERT INTO index_generations
		 (model, dimension, fingerprint, started_at, state)
		 VALUES ($1, $2, $3, $4, 'building')
		 RETURNING id`,
		model, dim, fp, now).Scan(&newID)
	if err != nil {
		if isUniqueViolation(err) {
			id, existingFP, ok, lookupErr := b.lookupBuilding(ctx)
			if lookupErr != nil {
				return 0, false, fmt.Errorf("lookup after insert race: %w", lookupErr)
			}
			if !ok {
				return 0, false, fmt.Errorf("insert generation: %w", err)
			}
			if existingFP != fp {
				return 0, false, fmt.Errorf("%w: existing building fingerprint=%q, requested=%q — activate or retire it before starting a new rebuild",
					vector.ErrBuildingInProgress, existingFP, fp)
			}
			return id, false, nil
		}
		return 0, false, fmt.Errorf("insert generation: %w", err)
	}
	return vector.GenerationID(newID), true, nil
}

func (b *Backend) lookupBuilding(ctx context.Context) (vector.GenerationID, string, bool, error) {
	var (
		id int64
		fp string
	)
	err := b.db.QueryRowContext(ctx,
		`SELECT id, fingerprint FROM index_generations WHERE state = 'building'`).
		Scan(&id, &fp)
	switch {
	case err == nil:
		return vector.GenerationID(id), fp, true, nil
	case errors.Is(err, sql.ErrNoRows):
		return 0, "", false, nil
	default:
		return 0, "", false, fmt.Errorf("lookup building generation: %w", err)
	}
}

// isUniqueViolation matches PostgreSQL's SQLSTATE 23505 via pgconn's
// typed error so locale-dependent message text cannot break detection.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "23505"
}

// afterSeedLockHook is a test-only synchronization seam. When non-nil it is
// invoked once inside seedPending's transaction AFTER the SET LOCAL
// statement_timeout reset but BEFORE the generation's state is re-read under
// the FOR NO KEY UPDATE row lock. It lets the concurrency regression test
// commit a RetireGeneration at exactly the window the orphan-pending race
// opens (seed tx begins → retire commits → seed re-reads + inserts), proving
// the locked re-validation refuses to seed a now-retired generation. It is
// always nil in production. Mirrors enqueue.go's afterGenSnapshotHook.
var afterSeedLockHook func()

// seedPending inserts one pending_embeddings row per live message in
// the main schema. Uses ON CONFLICT DO NOTHING for idempotency on
// retries and to deduplicate against rows already added by the
// concurrent Enqueuer path (parallel to sqlitevec's INSERT OR IGNORE).
//
// Because messages and pending_embeddings live in the same Postgres
// database, this can be done in a single INSERT … SELECT rather than
// streaming rows through Go like the SQLite backend does.
//
// The generation's state is re-read under a FOR NO KEY UPDATE row lock IN the
// same tx before the insert. This mirrors the Enqueuer's locked re-validation
// (enqueue.go): the lock conflicts with the no-key tuple lock that
// RetireGeneration / ActivateGeneration's state-flip UPDATE takes, so a
// concurrent retire that deletes this generation's pending rows and flips it
// to 'retired' cannot interleave with this seed to leave orphan pending rows
// behind. The two interleavings serialize:
//   - seed-first: retire's state-flip UPDATE blocks on this lock, then its
//     DELETE removes the rows we just inserted -> no orphan.
//   - retire-first: this locking SELECT blocks until retire commits, then
//     re-reads state='retired' and we skip the insert -> we seed nothing.
//
// Seedable = not yet retired, matching how the Enqueuer decides eligibility
// (WHERE state != 'retired'). A generation deleted outright (no row) is also
// skipped.
func (b *Backend) seedPending(ctx context.Context, gen vector.GenerationID, now int64) error {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin seed tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Disable the pool-wide 30s statement_timeout for this tx: the shared
	// store pool sets statement_timeout=30s via pgx RuntimeParams
	// (postgresConnConfig), and this single INSERT ... SELECT over the whole
	// messages table can exceed that on a 1M+ message archive (finding S1's
	// family, V7). SET LOCAL is tx-scoped and auto-resets on commit/rollback,
	// so the timeout cannot leak onto other connections. [V7]
	if _, err := tx.ExecContext(ctx, "SET LOCAL statement_timeout = 0"); err != nil {
		return fmt.Errorf("disable statement_timeout for seed: %w", err)
	}

	// Test-only synchronization seam (nil in production): fires after the tx
	// begins but before the locked re-read below, so the concurrency
	// regression test can commit a RetireGeneration inside the exact window
	// the orphan-pending race opens.
	if afterSeedLockHook != nil {
		afterSeedLockHook()
	}

	// Re-read the generation's state under a FOR NO KEY UPDATE row lock and
	// confirm it is still seedable (not retired) before inserting. If it has
	// been retired (or deleted) concurrently, skip the insert so we never seed
	// a retired generation with orphan pending rows. Held through the INSERT
	// below in this same tx.
	var state string
	err = tx.QueryRowContext(ctx,
		`SELECT state FROM index_generations WHERE id = $1 FOR NO KEY UPDATE`,
		int64(gen)).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		// Generation deleted concurrently — nothing to seed.
		return tx.Commit()
	}
	if err != nil {
		return fmt.Errorf("re-validate generation %d for seed: %w", gen, err)
	}
	if state == string(vector.GenerationRetired) {
		// Retired concurrently (PG: by a now-committed retire we just blocked
		// on) — do not seed, leaving no orphan pending rows for this gen.
		return tx.Commit()
	}

	stmt := fmt.Sprintf(`
		INSERT INTO pending_embeddings (generation_id, message_id, enqueued_at)
		SELECT $1, id, $2
		  FROM messages
		 WHERE %s
		ON CONFLICT (generation_id, message_id) DO NOTHING`,
		store.LiveMessagesWhere("", true))
	if _, err := tx.ExecContext(ctx, stmt, int64(gen), now); err != nil {
		return fmt.Errorf("seed pending: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit seed pending: %w", err)
	}
	return nil
}

// ActivateGeneration atomically retires the current active generation
// (if any) and promotes gen to active.
//
// Retiring the previously-active generation also DELETEs its embedding
// rows in the same transaction. The pgvector HNSW index is partial by
// dimension only (see migrate.go: WHERE dimension = N), so a single
// graph indexes every generation of that dimension; Search/FusedSearch
// post-filter by generation_id. Leaving a retired generation's vectors
// in the shared graph would consume the ef_search budget and erode the
// active generation's recall. Deleting them keeps the graph
// generation-clean. This intentionally differs from sqlitevec, whose
// vec0 PARTITION KEY isolates retired rows so it can retain them.
func (b *Backend) ActivateGeneration(ctx context.Context, gen vector.GenerationID, force bool) error {
	now := time.Now().Unix()
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin activate tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Disable the pool-wide 30s statement_timeout for this tx: the auto-retire
	// path below DELETEs the demoted generation's embeddings + pending rows,
	// which are corpus-size on a large archive and can exceed the shared store
	// pool's statement_timeout=30s, cancelling the activation at 30s and rolling
	// it back (finding C1, S1 family). SET LOCAL is tx-scoped and auto-resets on
	// commit/rollback, so the timeout cannot leak onto other connections. Must be
	// the first statement in the tx to cover every subsequent DELETE.
	if _, err := tx.ExecContext(ctx, "SET LOCAL statement_timeout = 0"); err != nil {
		return fmt.Errorf("disable statement_timeout for activate: %w", err)
	}

	// Demote the current active generation and capture its id in a single
	// statement via RETURNING, so the id whose embeddings we delete below is
	// provably the row this UPDATE retired (no separate non-locking SELECT that
	// a concurrent activation could race). No active row -> no row returned ->
	// demoted invalid -> the deletes are skipped, exactly as before. Done inside
	// the tx so the demote+delete is atomic with the activation below.
	var demoted sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`UPDATE index_generations
		    SET state = 'retired', completed_at = COALESCE(completed_at, $1)
		  WHERE state = 'active'
		  RETURNING id`, now).Scan(&demoted); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("retire previous active: %w", err)
	}
	if demoted.Valid {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM embeddings WHERE generation_id = $1`, demoted.Int64); err != nil {
			return fmt.Errorf("delete retired generation %d embeddings: %w", demoted.Int64, err)
		}
		// Reap the demoted generation's queue rows in the same tx. Retired
		// generations are never re-targeted by pickTarget, so any leftover
		// pending_embeddings rows would be orphaned forever (the
		// index_generations row is preserved, so the ON DELETE CASCADE never
		// fires). Deleting them keeps the documented stats invariant
		// ("retired generations have zero pending items") true. [cr2-3, cr2-4]
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM pending_embeddings WHERE generation_id = $1`, demoted.Int64); err != nil {
			return fmt.Errorf("delete retired generation %d pending: %w", demoted.Int64, err)
		}
	}
	// The promote re-checks the seeded/no-pending gate IN the same tx as the
	// flip (unless force). This closes the window between a CALLER's pre-flight
	// pending read and this UPDATE: no pending row committed before this
	// statement can sneak gen past the gate. It does NOT serialize against a
	// concurrent enqueue.go dual-enqueue under READ COMMITTED — the FK key-share
	// lock enqueue takes does not conflict with this non-key UPDATE, so an
	// enqueue that commits just AFTER this gated UPDATE can still leave one
	// pending row on the now-active gen. That post-flip row is acceptable: the
	// embed worker's active-generation top-up (see embed_job.go pickTarget /
	// enqueue.go) simply processes it on the next run. [cr2-1]
	res, err := tx.ExecContext(ctx,
		`UPDATE index_generations
		    SET state = 'active', activated_at = $1, completed_at = COALESCE(completed_at, $2)
		  WHERE id = $3 AND state = 'building'
		    AND ($4 OR seeded_at IS NOT NULL)
		    AND ($4 OR NOT EXISTS (
		        SELECT 1 FROM pending_embeddings WHERE generation_id = $3
		    ))`, now, now, int64(gen), force)
	if err != nil {
		return fmt.Errorf("activate: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return activateGateError(ctx, tx, gen, force)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit activate generation %d: %w", gen, err)
	}
	return nil
}

// activateGateError re-reads gen inside the activation tx to return a
// precise reason the gated promote affected zero rows: pending rows present,
// not finished seeding, unknown generation, or not in 'building' state.
// Mirrors the prior CLI raw helper so callers get the same actionable
// messages now that the gate lives in the backend.
func activateGateError(ctx context.Context, tx *sql.Tx, gen vector.GenerationID, force bool) error {
	var pending int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = $1`, int64(gen)).Scan(&pending); err != nil {
		return fmt.Errorf("count pending rows for generation %d: %w", gen, err)
	}
	if pending > 0 && !force {
		return fmt.Errorf("generation %d still has %d pending embedding rows; run `msgvault embeddings resume` or pass --force",
			gen, pending)
	}
	var state vector.GenerationState
	var seededAt sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT state, seeded_at FROM index_generations WHERE id = $1`, int64(gen)).Scan(&state, &seededAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %d", vector.ErrUnknownGeneration, gen)
		}
		return fmt.Errorf("lookup generation %d: %w", gen, err)
	}
	if state == vector.GenerationBuilding && !seededAt.Valid && !force {
		return fmt.Errorf("generation %d has not finished seeding; run `msgvault embeddings resume` or pass --force",
			gen)
	}
	return fmt.Errorf("generation %d not in 'building' state", gen)
}

// RetireGeneration marks the given generation as retired and DELETEs its
// embedding rows in one transaction.
//
// The embedding rows are deleted for the same reason as in
// ActivateGeneration's auto-retire path: pgvector's HNSW index is partial
// by dimension only, so all generations of a dimension share one graph and
// Search/FusedSearch post-filter by generation_id. Retaining a retired
// generation's vectors would consume the ef_search budget and erode the
// active generation's recall. The index_generations row is preserved
// (state='retired'); only its embeddings are removed. This intentionally
// differs from sqlitevec, whose vec0 PARTITION KEY isolates retired rows.
//
// Unless force is true, the state-flip UPDATE refuses to retire a generation
// in state='active' (WHERE state != 'active'): if it affects zero rows the
// active guard tripped, so the tx rolls back returning ErrRefuseRetireActive
// WITHOUT touching embeddings or pending rows. The guard lives in the same tx
// as the flip — closing the CLI's pre-flight TOCTOU so a concurrent
// activation cannot delete the now-serving generation's embeddings without
// --force-active. force retires unconditionally (operator override).
func (b *Backend) RetireGeneration(ctx context.Context, gen vector.GenerationID, force bool) error {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin retire tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Disable the pool-wide 30s statement_timeout for this tx: the DELETEs below
	// remove the retired generation's embeddings + pending rows, which are
	// corpus-size on a large archive and can exceed the shared store pool's
	// statement_timeout=30s, cancelling the retire at 30s and rolling it back
	// (finding C1, S1 family). SET LOCAL is tx-scoped and auto-resets on
	// commit/rollback, so the timeout cannot leak onto other connections. Must be
	// the first statement in the tx to cover every subsequent DELETE.
	if _, err := tx.ExecContext(ctx, "SET LOCAL statement_timeout = 0"); err != nil {
		return fmt.Errorf("disable statement_timeout for retire: %w", err)
	}

	// The active-gen guard is the WHERE clause itself: when force is false we
	// only retire a generation that is NOT active, so a concurrent activation
	// that flipped gen to active between the caller's pre-flight read and this
	// statement leaves zero rows affected and we bail out before deleting
	// anything. force=true drops the guard (true OR ... is always satisfiable).
	res, err := tx.ExecContext(ctx,
		`UPDATE index_generations SET state = 'retired'
		  WHERE id = $1 AND ($2 OR state != 'active')`, int64(gen), force)
	if err != nil {
		return fmt.Errorf("retire generation %d: %w", gen, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return retireGateError(ctx, tx, gen, force)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM embeddings WHERE generation_id = $1`, int64(gen)); err != nil {
		return fmt.Errorf("delete retired generation %d embeddings: %w", gen, err)
	}
	// Reap the retired generation's queue rows in the same tx so they cannot
	// be orphaned (no future run re-targets a retired generation, and the
	// preserved index_generations row means the ON DELETE CASCADE never
	// fires). Keeps the "retired generations have zero pending items"
	// stats invariant true. [cr2-2, cr2-3]
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM pending_embeddings WHERE generation_id = $1`, int64(gen)); err != nil {
		return fmt.Errorf("delete retired generation %d pending: %w", gen, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit retire generation %d: %w", gen, err)
	}
	return nil
}

// retireGateError re-reads gen inside the retire tx to explain why the gated
// state flip affected zero rows: the generation is active (and force was not
// passed), it is already retired (idempotent no-op, not an error), or it does
// not exist. Mirrors activateGateError so the management command gets precise,
// actionable errors now that the guard lives in the backend.
func retireGateError(ctx context.Context, tx *sql.Tx, gen vector.GenerationID, force bool) error {
	var state vector.GenerationState
	if err := tx.QueryRowContext(ctx,
		`SELECT state FROM index_generations WHERE id = $1`, int64(gen)).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %d", vector.ErrUnknownGeneration, gen)
		}
		return fmt.Errorf("lookup generation %d: %w", gen, err)
	}
	if state == vector.GenerationActive && !force {
		return fmt.Errorf("%w: generation %d", vector.ErrRefuseRetireActive, gen)
	}
	// A non-active row always matches `state != 'active'`, so the gated UPDATE
	// would have affected it (a no-op flip still counts as a matched row on
	// both backends). Reaching here for a non-active, existing generation means
	// the row vanished mid-tx; surface it rather than reporting a phantom retire.
	return fmt.Errorf("retire generation %d: state flip affected no rows (state=%q)", gen, state)
}

// ActiveGeneration returns the current active generation, or
// vector.ErrNoActiveGeneration if none exists.
func (b *Backend) ActiveGeneration(ctx context.Context) (vector.Generation, error) {
	return b.generationByState(ctx, vector.GenerationActive)
}

// BuildingGeneration returns the current building generation, or nil
// if none exists.
func (b *Backend) BuildingGeneration(ctx context.Context) (*vector.Generation, error) {
	g, err := b.generationByState(ctx, vector.GenerationBuilding)
	if errors.Is(err, vector.ErrNoActiveGeneration) {
		return nil, nil //nolint:nilnil // (nil, nil) signals "no building generation"; callers nil-check the pointer
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

func (b *Backend) generationByState(ctx context.Context, state vector.GenerationState) (vector.Generation, error) {
	var g vector.Generation
	var startedAt int64
	var completedAt, activatedAt sql.NullInt64
	err := b.db.QueryRowContext(ctx,
		`SELECT id, model, dimension, fingerprint, state,
		        started_at, completed_at, activated_at, message_count
		   FROM index_generations WHERE state = $1`, string(state)).Scan(
		&g.ID, &g.Model, &g.Dimension, &g.Fingerprint, &g.State,
		&startedAt, &completedAt, &activatedAt, &g.MessageCount)
	if errors.Is(err, sql.ErrNoRows) {
		return vector.Generation{}, vector.ErrNoActiveGeneration
	}
	if err != nil {
		return vector.Generation{}, err
	}
	g.StartedAt = time.Unix(startedAt, 0)
	if completedAt.Valid {
		t := time.Unix(completedAt.Int64, 0)
		g.CompletedAt = &t
	}
	if activatedAt.Valid {
		t := time.Unix(activatedAt.Int64, 0)
		g.ActivatedAt = &t
	}
	return g, nil
}

// Upsert writes chunks to the given generation. Transactional.
// Dimension is verified per-chunk against the generation's recorded
// dimension. Mirrors sqlitevec.Upsert semantics: ErrUnknownGeneration
// when gen is missing, ErrDimensionMismatch when any chunk's length
// disagrees with the generation's dimension.
func (b *Backend) Upsert(ctx context.Context, gen vector.GenerationID, chunks []vector.Chunk) error {
	if len(chunks) == 0 {
		return nil
	}

	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin upsert tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Read the generation's dimension and lifecycle state under a row lock
	// inside the SAME transaction that writes the embeddings. FOR UPDATE
	// serializes this Upsert against ActivateGeneration/RetireGeneration,
	// which UPDATE this same index_generations row in their txs. This closes
	// the race where a stale worker (whose claims were reclaimed) or an
	// operator `embeddings retire --force-active` retires+deletes the
	// generation and the Upsert then re-inserts its vectors into the shared
	// HNSW graph (re-pollution) and inflates message_count. A retire that
	// commits first makes this read observe state='retired' and bail; an
	// Upsert that commits first blocks the retire until done (its subsequent
	// DELETE then cleans the just-written rows).
	var dim int
	var state string
	err = tx.QueryRowContext(ctx,
		`SELECT dimension, state FROM index_generations WHERE id = $1 FOR UPDATE`,
		int64(gen)).Scan(&dim, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %d", vector.ErrUnknownGeneration, gen)
	}
	if err != nil {
		return fmt.Errorf("lookup generation %d: %w", gen, err)
	}
	if state == string(vector.GenerationRetired) {
		return fmt.Errorf("%w: %d", vector.ErrGenerationRetired, gen)
	}
	for _, c := range chunks {
		if len(c.Vector) != dim {
			return fmt.Errorf("%w: chunk for msg %d has %d dims, gen has %d",
				vector.ErrDimensionMismatch, c.MessageID, len(c.Vector), dim)
		}
	}

	// message_count tracks distinct messages, not chunks. Count how many
	// of the batch's message_ids already have any row in the generation
	// so we can apply an O(1) delta after the replace below.
	distinctIDs := distinctMessageIDs(chunks)
	preexisting, err := countExistingMessagesTx(ctx, tx, gen, distinctIDs)
	if err != nil {
		return err
	}

	// Idempotency: clear any prior chunks for the message_ids we're about
	// to (re)write before inserting the new chunk set. Chunking is not
	// stable across upserts — the same message may have produced 3 chunks
	// last time and 2 this time — so a plain per-chunk upsert would leave
	// orphaned tail chunks behind. Mirrors sqlitevec's replace semantics.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM embeddings
		  WHERE generation_id = $1 AND message_id = ANY($2::bigint[])`,
		int64(gen), int64Array(distinctIDs)); err != nil {
		return fmt.Errorf("clear prior chunks: %w", err)
	}

	now := time.Now().Unix()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO embeddings
		  (generation_id, message_id, chunk_index, embedded_at, source_char_len,
		   chunk_char_start, chunk_char_end, truncated, dimension, embedding)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::vector)`)
	if err != nil {
		return fmt.Errorf("prepare embeddings insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, c := range chunks {
		if _, err := stmt.ExecContext(ctx,
			int64(gen), c.MessageID, c.ChunkIndex, now, c.SourceCharLen,
			c.ChunkCharStart, c.ChunkCharEnd, c.Truncated, dim,
			vectorLiteral(c.Vector),
		); err != nil {
			return fmt.Errorf("insert embedding for msg %d chunk %d: %w", c.MessageID, c.ChunkIndex, err)
		}
	}

	delta := len(distinctIDs) - preexisting
	if err := applyMessageCountDeltaTx(ctx, tx, gen, delta); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit upsert for generation %d: %w", gen, err)
	}
	return nil
}

// distinctMessageIDs returns the unique message_ids referenced by chunks,
// preserving first-seen order. Mirrors the sqlitevec helper.
func distinctMessageIDs(chunks []vector.Chunk) []int64 {
	seen := make(map[int64]struct{}, len(chunks))
	out := make([]int64, 0, len(chunks))
	for _, c := range chunks {
		if _, ok := seen[c.MessageID]; ok {
			continue
		}
		seen[c.MessageID] = struct{}{}
		out = append(out, c.MessageID)
	}
	return out
}

func applyMessageCountDeltaTx(ctx context.Context, tx *sql.Tx, gen vector.GenerationID, delta int) error {
	if delta == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE index_generations SET message_count = message_count + $1 WHERE id = $2`,
		delta, int64(gen)); err != nil {
		return fmt.Errorf("update message_count: %w", err)
	}
	return nil
}

// countExistingMessagesTx returns how many of the given message_ids
// already have at least one embedding row in the generation. Counts
// DISTINCT messages (not chunk rows) so message_count deltas stay
// message-scoped even when a message spans multiple chunks.
func countExistingMessagesTx(ctx context.Context, tx *sql.Tx, gen vector.GenerationID, ids []int64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var n int
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT message_id) FROM embeddings
		  WHERE generation_id = $1 AND message_id = ANY($2::bigint[])`,
		int64(gen), int64Array(ids)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count existing messages: %w", err)
	}
	return n, nil
}

// vectorLiteral formats a float32 slice as pgvector's text input
// format, e.g. "[1.0,2.5,-3.14]". Bound via $N::vector this works
// without the pgvector-go binding, which keeps the dependency surface
// minimal — pgx already ships in this repo.
func vectorLiteral(v []float32) string {
	var sb strings.Builder
	sb.Grow(len(v) * 8)
	sb.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	sb.WriteByte(']')
	return sb.String()
}

// int64Array formats an int64 slice as the PostgreSQL array literal
// "{1,2,3}". Bound via $N::bigint[].
func int64Array(ids []int64) string {
	var sb strings.Builder
	sb.Grow(len(ids) * 8)
	sb.WriteByte('{')
	for i, id := range ids {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatInt(id, 10))
	}
	sb.WriteByte('}')
	return sb.String()
}

// textArray formats a string slice as a PostgreSQL array literal.
// Bound via $N::text[].
func textArray(values []string) string {
	var sb strings.Builder
	sb.WriteByte('{')
	for i, v := range values {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteByte('"')
		for _, r := range v {
			if r == '"' || r == '\\' {
				sb.WriteByte('\\')
			}
			sb.WriteRune(r)
		}
		sb.WriteByte('"')
	}
	sb.WriteByte('}')
	return sb.String()
}

// parseVectorLiteral decodes pgvector's text output ("[1,2,3]") back
// into a []float32 of length dim. Returns an error if the row reports
// a different number of components — guards against accidentally
// loading a vector from a different generation.
func parseVectorLiteral(s string, dim int) ([]float32, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return nil, fmt.Errorf("malformed vector literal: %q", s)
	}
	body := strings.TrimSpace(s[1 : len(s)-1])
	if body == "" {
		if dim != 0 {
			return nil, fmt.Errorf("vector is empty, want %d dims", dim)
		}
		return nil, nil
	}
	parts := strings.Split(body, ",")
	if len(parts) != dim {
		return nil, fmt.Errorf("vector has %d dims, want %d", len(parts), dim)
	}
	out := make([]float32, dim)
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return nil, fmt.Errorf("parse vector component %d: %w", i, err)
		}
		out[i] = float32(f)
	}
	return out, nil
}

// LoadVector returns the embedding for messageID in the active
// generation. Mirrors sqlitevec.LoadVector.
func (b *Backend) LoadVector(ctx context.Context, messageID int64) ([]float32, error) {
	active, err := b.ActiveGeneration(ctx)
	if err != nil {
		return nil, err
	}
	// Return the chunk_index = 0 vector — the head of the message, which
	// always exists for any embedded message regardless of how many
	// additional chunks it has. find_similar (the only LoadVector caller
	// today) treats embeddings as message-level. Mirrors sqlitevec.
	var lit string
	err = b.db.QueryRowContext(ctx,
		`SELECT embedding::text FROM embeddings
		  WHERE generation_id = $1 AND message_id = $2 AND chunk_index = 0`,
		int64(active.ID), messageID).Scan(&lit)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("no embedding for message %d in generation %d", messageID, active.ID)
	}
	if err != nil {
		return nil, fmt.Errorf("load vector for message %d: %w", messageID, err)
	}
	return parseVectorLiteral(lit, active.Dimension)
}

// Search runs an ANN query against the given generation and returns
// the top-k hits (optionally intersected with a structured filter).
// Uses pgvector's cosine-distance operator (<=>), which returns 0..2;
// hits are emitted with Score = 1 - distance to align with the
// sqlitevec convention.
func (b *Backend) Search(ctx context.Context, gen vector.GenerationID, queryVec []float32, k int, filter vector.Filter) ([]vector.Hit, error) {
	if len(queryVec) == 0 {
		return nil, errors.New("search: empty query vector")
	}
	var dim int
	err := b.db.QueryRowContext(ctx,
		`SELECT dimension FROM index_generations WHERE id = $1`, int64(gen)).Scan(&dim)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %d", vector.ErrUnknownGeneration, gen)
	}
	if err != nil {
		return nil, fmt.Errorf("lookup generation %d: %w", gen, err)
	}
	if len(queryVec) != dim {
		return nil, fmt.Errorf("%w: query has %d dims, gen has %d",
			vector.ErrDimensionMismatch, len(queryVec), dim)
	}

	// chunkCeiling is the actual number of embedding rows in the
	// generation — the upper bound for the candidate-widening loops below.
	// When the inner ANN LIMIT already reaches this value, no wider fetch
	// can surface more distinct messages, so the loop terminates.
	chunkCeiling, err := b.chunkCount(ctx, gen)
	if err != nil {
		return nil, err
	}
	if chunkCeiling == 0 {
		return nil, nil
	}

	// Filter resolution. Unlike sqlitevec, embeddings live in the same
	// database as messages — we can express the live-message and
	// structured filters as a single SQL query against both tables
	// without ATTACHing or json_each tricks. Empty filter still benefits
	// from a slim fast path that elides the join.
	queryVecLit := vectorLiteral(queryVec)
	if filter.IsEmpty() {
		// Fast path: let pgvector use the HNSW index by issuing ORDER BY
		// <=> LIMIT inside a subquery first. The inner SELECT returns at
		// most innerLimit rows in ANN order; the HNSW index applies to that
		// inner ORDER BY (ef_search is raised at connect time — see
		// store.HNSWEfSearch — so the index is not capped at the pgvector
		// default of 40). The outer query re-groups by message_id to
		// collapse multi-chunk messages (best chunk wins via MIN), then
		// re-sorts and re-limits the deduplicated result. We start at
		// k*annOverFetchFactor chunks so the common single-chunk case is a
		// single query; on multi-chunk corpora the dedup can collapse the
		// candidate set below k, so the loop doubles the inner LIMIT
		// (bounded by chunkCeiling) until k distinct messages survive.
		//
		// The dimension predicate is embedded as a literal (matching the
		// partial HNSW index's WHERE dimension = <N> in migrate.go and the
		// fused.go ANN path) rather than a bind param, so a PG generic plan
		// can prove the partial-index predicate and use the HNSW index.
		stmt := fmt.Sprintf(`
			SELECT ann.message_id,
			       MIN(ann.distance) AS distance
			  FROM (
			        SELECT e.message_id,
			               (e.embedding::vector(%[1]d)) <=> $1::vector AS distance
			          FROM embeddings e
			         WHERE e.generation_id = $2
			           AND e.dimension = %[1]d
			           AND EXISTS (
			                SELECT 1 FROM messages m
			                 WHERE m.id = e.message_id AND %[2]s)
			         ORDER BY e.embedding::vector(%[1]d) <=> $1::vector
			         LIMIT $3
			       ) ann
			 GROUP BY ann.message_id
			 ORDER BY distance, ann.message_id
			 LIMIT $4`, dim, store.LiveMessagesWhere("m", true))
		// Empty-filter path: the candidate universe is the whole
		// generation, so the inner-LIMIT ceiling is the generation chunk
		// count and there is no separate distinct-message early exit
		// (passing k makes that check a no-op beyond len(hits) >= k).
		return searchWiden(k, chunkCeiling, k, func(innerLimit int) ([]vector.Hit, error) {
			return b.scanHits(ctx, stmt, queryVecLit, int64(gen), innerLimit, k)
		})
	}

	// Filtered path: HNSW cannot be applied when intersecting with a
	// structured filter, so we use the same inner-subquery shape as the
	// empty-filter path but accept a sequential scan within the filtered
	// set. Rather than materializing every matching message id in Go and
	// shipping it back as one bigint[] param (which serialized hundreds of
	// thousands of ids per query on a broad filter), the filter stays in
	// SQL as an inline correlated EXISTS against messages — the same shape
	// the empty-filter fast path uses for liveness (backend.go ~816-818),
	// extended with the structured-filter clauses. [V2]
	//
	// The inner ORDER BY <=> LIMIT still short-circuits on chunk count.
	// The widening loop mirrors the empty-filter (and sqlitevec) pattern
	// so a multi-chunk filtered universe still reaches k distinct messages.
	//
	// The inner LIMIT counts CHUNKS, so the loop ceiling must be a CHUNK
	// count, not a message count: a MESSAGE count would under-shoot on
	// multi-chunk filtered corpora — the inner LIMIT would saturate at the
	// message count before the GROUP BY surfaced k distinct messages,
	// short-returning (the exact failure sqlitevec's comment warns
	// against). Bound the loop by the CHUNK count of the filtered set, and
	// use the DISTINCT filtered-message count (messages with a chunk in
	// this generation) only as an early exit so a selective filter does not
	// drive the loop to the full generation. Both counts are derived from
	// the SAME EXISTS predicate the search SQL applies, so the loop bounds
	// stay exactly aligned with what the inner scan can surface.
	//
	// Recompute the loop ceilings (chunk count + distinct-message count)
	// over the SAME EXISTS predicate, so the loop bounds stay exactly
	// aligned with what the inner scan can surface. Both the count query
	// and the search query rebuild the EXISTS clause with their own bind
	// closure, so each statement's $N placeholders resolve against its own
	// ordinals.
	filteredChunks, filteredMessages, err := b.filteredChunkAndMessageCount(ctx, gen, filter)
	if err != nil {
		return nil, err
	}
	if filteredChunks == 0 {
		return nil, nil
	}

	// args carries the stable prefix shared across widening runs: $1 =
	// query vector, $2 = generation, $3 = dimension, $4.. = the structured
	// filter's bound values. The widening loop appends the two trailing
	// args (inner LIMIT, outer LIMIT) per run.
	baseArgs := []any{queryVecLit, int64(gen), int64(dim)}
	bind := func(v any) string {
		baseArgs = append(baseArgs, v)
		return fmt.Sprintf("$%d", len(baseArgs))
	}
	existsClause := filterExistsClause("e", filter, bind)
	innerArg := fmt.Sprintf("$%d", len(baseArgs)+1)
	outerArg := fmt.Sprintf("$%d", len(baseArgs)+2)
	stmt := fmt.Sprintf(`
		SELECT ann.message_id,
		       MIN(ann.distance) AS distance
		  FROM (
		        SELECT e.message_id,
		               (e.embedding::vector(%[1]d)) <=> $1::vector AS distance
		          FROM embeddings e
		         WHERE e.generation_id = $2
		           AND e.dimension = $3
		           AND %[2]s
		         ORDER BY e.embedding::vector(%[1]d) <=> $1::vector
		         LIMIT %[3]s
		       ) ann
		 GROUP BY ann.message_id
		 ORDER BY distance, ann.message_id
		 LIMIT %[4]s`, dim, existsClause, innerArg, outerArg)
	return searchWiden(k, filteredChunks, filteredMessages, func(innerLimit int) ([]vector.Hit, error) {
		runArgs := append(append([]any(nil), baseArgs...), innerLimit, k)
		return b.scanHits(ctx, stmt, runArgs...)
	})
}

// filterExistsClause returns a correlated EXISTS predicate that constrains
// an embeddings row (joined via embedAlias.message_id) to a live message
// matching the structured filter. It mirrors the empty-filter fast path's
// inline liveness EXISTS (backend.go ~816-818) but adds the structured
// filter clauses, keeping the whole filter in SQL instead of round-tripping
// matching ids through Go. Filter values are bound via the supplied bind
// closure; the live + filter clauses all reference the inner alias `m`. [V2].
func filterExistsClause(embedAlias string, f vector.Filter, bind func(any) string) string {
	clauses := append([]string{store.LiveMessagesWhere("m", true)}, buildPGFilterClauses(f, bind)...)
	return fmt.Sprintf(
		"EXISTS (SELECT 1 FROM messages m WHERE m.id = %s.message_id AND %s)",
		embedAlias, strings.Join(clauses, " AND "))
}

// filteredChunkAndMessageCount returns, for the given generation and
// structured filter, (a) the number of embedding CHUNKS whose message
// satisfies the live + filter predicate and (b) the number of DISTINCT such
// messages that have a chunk in the generation. The chunk count is the
// inner-LIMIT ceiling for the filtered widening loop (the LIMIT counts
// chunks); the distinct-message count is the early exit. Both come from a
// single scan and use the EXACT EXISTS predicate the search SQL applies, so
// the loop bounds match what the inner scan can surface. The clause is
// rebuilt with this statement's own bind closure ($1 = generation, $2.. =
// filter values). [V2].
func (b *Backend) filteredChunkAndMessageCount(ctx context.Context, gen vector.GenerationID, f vector.Filter) (chunks, messages int, err error) {
	args := []any{int64(gen)}
	bind := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	existsClause := filterExistsClause("e", f, bind)
	q := `SELECT COUNT(*), COUNT(DISTINCT e.message_id)
		   FROM embeddings e
		  WHERE e.generation_id = $1 AND ` + existsClause
	if err := b.db.QueryRowContext(ctx, q, args...).Scan(&chunks, &messages); err != nil {
		return 0, 0, fmt.Errorf("lookup filtered chunk count: %w", err)
	}
	return chunks, messages, nil
}

// chunkCount returns the number of embedding rows in the generation.
// It is the upper bound for the candidate-widening loop: once the inner
// ANN LIMIT reaches it, no wider fetch can surface more distinct messages.
func (b *Backend) chunkCount(ctx context.Context, gen vector.GenerationID) (int, error) {
	var n int
	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = $1`,
		int64(gen)).Scan(&n); err != nil {
		return 0, fmt.Errorf("lookup chunk count: %w", err)
	}
	return n, nil
}

// searchWiden runs the inner ANN scan with a doubling inner LIMIT until
// at least k distinct messages survive the outer GROUP BY dedup, the
// distinct-message early exit is reached, the candidate ceiling is
// reached, or no further widening is possible. Mirrors sqlitevec.Search's
// widening loop. The common single-chunk case is satisfied by the first
// fetch (k*annOverFetchFactor) so it stays a single query; only
// multi-chunk corpora trigger additional passes.
//
// ceiling counts CHUNKS (the inner LIMIT operates on embedding rows): it
// is the chunk count of the candidate universe (the whole generation for
// the empty-filter path, or just the filtered set for the filtered path)
// and bounds the inner LIMIT so the loop always terminates. Passing a
// MESSAGE count here would under-shoot on multi-chunk corpora — the inner
// LIMIT would saturate before the GROUP BY has surfaced k distinct
// messages — which is the bug sqlitevec's own comment warns against.
//
// distinctEarlyExit counts distinct MESSAGES that can possibly appear in
// the result (e.g. distinct filtered messages that have a chunk in this
// generation). It is an early exit only: once len(hits) reaches it, every
// candidate message is already in the result with its best-distance
// chunk, so further widening cannot change the answer. For the
// empty-filter path it equals k, making the check a no-op beyond the
// existing len(hits) >= k condition.
func searchWiden(k, ceiling, distinctEarlyExit int, run func(innerLimit int) ([]vector.Hit, error)) ([]vector.Hit, error) {
	innerLimit := max(k*annOverFetchFactor, k)
	for {
		if innerLimit > ceiling {
			innerLimit = ceiling
		}
		hits, err := run(innerLimit)
		if err != nil {
			return nil, err
		}
		if len(hits) >= k || len(hits) >= distinctEarlyExit || innerLimit >= ceiling {
			if len(hits) > k {
				hits = hits[:k]
			}
			// Re-rank so callers see contiguous 1,2,3… ranks.
			for i := range hits {
				hits[i].Rank = i + 1
			}
			return hits, nil
		}
		innerLimit *= 2
	}
}

func (b *Backend) scanHits(ctx context.Context, query string, args ...any) ([]vector.Hit, error) {
	rows, err := b.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ann query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var hits []vector.Hit
	for i := 1; rows.Next(); i++ {
		var id int64
		var dist float64
		if err := rows.Scan(&id, &dist); err != nil {
			return nil, fmt.Errorf("scan hit: %w", err)
		}
		hits = append(hits, vector.Hit{
			MessageID: id,
			Score:     1.0 - dist,
			Rank:      i,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate hits: %w", err)
	}
	return hits, nil
}

// Delete removes the given messages from the specified generation in
// one transaction. Empty messageIDs is a no-op.
func (b *Backend) Delete(ctx context.Context, gen vector.GenerationID, messageIDs []int64) error {
	if len(messageIDs) == 0 {
		return nil
	}

	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Take the index_generations row lock FIRST, mirroring Upsert's
	// `SELECT ... FOR UPDATE` (backend.go ~437). Every other write path
	// (Upsert, ActivateGeneration, RetireGeneration) acquires this row
	// before touching the embeddings rows; if Delete instead locked
	// embeddings first (as it did before — the dimension lookup was outside
	// the tx and the message_count UPDATE locked index_generations only at
	// the end) it would create an ABBA deadlock asymmetry with those
	// writers on the same generation. Locking here also closes the TOCTOU
	// where the generation could be retired/deleted out from under this
	// Delete. The dimension value is unused by the deletion itself but the
	// locked read also yields ErrUnknownGeneration semantics.
	var dim int
	err = tx.QueryRowContext(ctx,
		`SELECT dimension FROM index_generations WHERE id = $1 FOR UPDATE`, int64(gen)).Scan(&dim)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %d", vector.ErrUnknownGeneration, gen)
	}
	if err != nil {
		return fmt.Errorf("lookup generation %d: %w", gen, err)
	}

	willDelete, err := countExistingMessagesTx(ctx, tx, gen, messageIDs)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM embeddings
		  WHERE generation_id = $1 AND message_id = ANY($2::bigint[])`,
		int64(gen), int64Array(messageIDs)); err != nil {
		return fmt.Errorf("delete embeddings: %w", err)
	}
	if err := applyMessageCountDeltaTx(ctx, tx, gen, -willDelete); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete tx: %w", err)
	}
	return nil
}

// Stats returns counts for the given generation. When gen == 0,
// counts are aggregated across all generations. StorageBytes is the
// total size of the embeddings table (pg_total_relation_size) — a
// single table-wide value across generations, which the caller can
// interpret with that caveat.
func (b *Backend) Stats(ctx context.Context, gen vector.GenerationID) (vector.Stats, error) {
	var s vector.Stats
	where := "WHERE generation_id = $1"
	args := []any{int64(gen)}
	if gen == 0 {
		where, args = "", nil
	} else {
		var exists int
		err := b.db.QueryRowContext(ctx,
			`SELECT 1 FROM index_generations WHERE id = $1`, int64(gen)).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return s, fmt.Errorf("%w: %d", vector.ErrUnknownGeneration, gen)
		}
		if err != nil {
			return s, fmt.Errorf("lookup generation %d: %w", gen, err)
		}
	}

	// EmbeddingCount is distinct messages, not chunk rows — a long message
	// occupies multiple rows but counts as one embedded message, matching
	// the sqlitevec semantics the progress/summary code relies on. The
	// aggregate path (gen == 0) counts DISTINCT (generation_id, message_id)
	// so a message embedded in two generations counts as two units of work.
	embeddingCountSQL := `SELECT COUNT(DISTINCT message_id) FROM embeddings ` + where
	if gen == 0 {
		embeddingCountSQL = `SELECT COUNT(*) FROM (SELECT DISTINCT generation_id, message_id FROM embeddings) s`
	}
	if err := b.db.QueryRowContext(ctx, embeddingCountSQL, args...).Scan(&s.EmbeddingCount); err != nil {
		return s, fmt.Errorf("count embeddings: %w", err)
	}
	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings `+where, args...).Scan(&s.PendingCount); err != nil {
		return s, fmt.Errorf("count pending: %w", err)
	}
	// StorageBytes: total on-disk size of the embeddings table (heap +
	// indexes + TOAST), table-wide rather than per-generation. Unlike
	// sqlitevec (whose caller derives size from the vectors.db file),
	// pgvector embeddings share the main database, so the backend is the
	// only place that can report this. to_regclass guards a not-yet-
	// migrated table so Stats never errors.
	if err := b.db.QueryRowContext(ctx,
		`SELECT COALESCE(pg_total_relation_size(to_regclass('embeddings')), 0)`).Scan(&s.StorageBytes); err != nil {
		return s, fmt.Errorf("embeddings storage size: %w", err)
	}
	return s, nil
}
