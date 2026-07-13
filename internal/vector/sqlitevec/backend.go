//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

// sqliteDatetimeFormat is the text DATETIME layout used everywhere
// else in the repository (see internal/query/sqlite.go). Bind date
// bounds with this format so boundary comparisons are consistent
// with the existing query paths.
const sqliteDatetimeFormat = "2006-01-02 15:04:05"

// Compile-time check that *Backend satisfies the vector.Backend interface.
var _ vector.Backend = (*Backend)(nil)
var _ vector.ChunkScoringBackend = (*Backend)(nil)

// Options configures how Open establishes a Backend.
type Options struct {
	Path       string
	MainPath   string            // filesystem path to msgvault.db; required for FusedSearch
	Dimension  int               // default dimension for EnsureVectorTable at open
	MainDB     *sql.DB           // handle to the main msgvault.db
	BuildScope vector.BuildScope // empty means full corpus
	// ReadOnly indicates the main DB handle (MainDB) was opened read-only
	// — e.g. the MCP server's store.OpenReadOnly (_query_only=true). When
	// set, Open SKIPS BackfillEmbedGenForUpgrade, which would otherwise
	// WRITE messages.embed_gen + applied_migrations through the read-only
	// main handle and fail. This mirrors pgvector.Options.SkipMigrate's
	// read-only guard. Migrate still runs because it only writes vectors.db,
	// which is opened read-write regardless.
	ReadOnly bool
}

// Backend implements vector.Backend and vector.FusingBackend against a
// SQLite database with the sqlite-vec extension.
type Backend struct {
	db       *sql.DB // handle to vectors.db
	mainDB   *sql.DB // handle to msgvault.db
	path     string  // filesystem path to vectors.db
	mainPath string  // filesystem path to msgvault.db (for ATTACH)
	dim      int
	scope    vector.BuildScope
	// readOnly is true when mainDB was opened read-only (MCP). The
	// one-time upgrade backfill self-guards on it so it never writes
	// through the read-only main handle. See Options.ReadOnly.
	readOnly bool
}

// Open opens vectors.db, runs migrations, and retains the main database
// handle for seed queries. Caller must call Close.
func Open(ctx context.Context, opts Options) (*Backend, error) {
	if err := RegisterExtension(); err != nil {
		return nil, err
	}
	db, err := sql.Open(DriverName(), opts.Path)
	if err != nil {
		return nil, fmt.Errorf("open vectors.db: %w", err)
	}
	if err := Migrate(ctx, db, opts.Dimension); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate vectors.db: %w", err)
	}
	b := &Backend{
		db:       db,
		mainDB:   opts.MainDB,
		path:     opts.Path,
		mainPath: opts.MainPath,
		dim:      opts.Dimension,
		scope:    vector.NewBuildScope(opts.BuildScope.MessageTypes),
		readOnly: opts.ReadOnly,
	}
	// Orphaned-stamp reset (vectors.db-recreate safety): clear embed_gen for
	// any message whose stamp points to a generation id that no longer exists
	// in index_generations. This MUST run BEFORE BackfillEmbedGenForUpgrade so
	// a freshly recreated vectors.db (empty index_generations, ids restarting
	// at 1) cannot reuse an old gen id whose stale stamps would mask coverage.
	// Not ledger-guarded: a recreate can happen between any two process
	// starts, so it re-checks on every writable Open (cheap + idempotent).
	// Self-guards on b.mainDB == nil / b.readOnly exactly like the backfill.
	if err := b.resetOrphanedEmbedGen(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("reset orphaned embed_gen: %w", err)
	}
	// One-time upgrade backfill (Package A): stamp embed_gen for messages
	// already embedded under the active generation, so an upgraded v0.14–
	// v0.15 archive does not read as entirely missing and trigger a full
	// re-embed. Ledger-guarded, so it runs at most once. No-ops when the
	// main DB handle is absent (management commands), already applied, or
	// the main handle is read-only (MCP) — the backfill self-guards on
	// b.readOnly so it never WRITES through a query-only main handle. Migrate
	// above still ran: it only writes vectors.db, which is read-write here.
	if err := b.BackfillEmbedGenForUpgrade(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("embed_gen upgrade backfill: %w", err)
	}
	// Drop the dead pending_embeddings queue table now that the backfill has
	// consulted it: the backfill preserves the table's legacy
	// re-embed signal, then we drop it here. Gated to the writable path —
	// mirrors the backfill's own b.readOnly / b.mainDB guards — so a read-only
	// Open leaves the table (and its signal) for a later writable open. Skipped
	// when the main handle is absent (management commands) so a backend opened
	// without a writable main DB does not mutate vectors.db schema unexpectedly.
	// Idempotent.
	if b.mainDB != nil && !b.readOnly {
		if err := b.dropDeadPendingEmbeddings(ctx); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("drop dead pending_embeddings: %w", err)
		}
	}
	return b, nil
}

// Close releases the vectors.db handle.
func (b *Backend) Close() error { return b.db.Close() }

// DB returns the underlying *sql.DB for vectors.db. Exposed for callers
// that need to share the pool (e.g. the embed worker's VectorsDB field).
func (b *Backend) DB() *sql.DB { return b.db }

// Path returns the filesystem path of vectors.db.
func (b *Backend) Path() string { return b.path }

// CreateGeneration allocates a new building generation (§5.1 of the
// spec). Under the scan-and-fill design there is no pending_embeddings
// seed: a building generation is just a row, and the embed worker
// populates it by scanning messages whose embed_gen does not yet match
// the generation. seeded_at is stamped at creation as harmless vestigial
// metadata (it no longer gates a seed pass and no longer gates
// activation; coverage is the real gate).
//
// If a building generation already exists with the same fingerprint,
// returns its id so a crashed or interrupted rebuild can resume —
// scan-and-fill simply continues from wherever the previous attempt left
// off (covered rows are skipped by the scan predicate). A mismatched
// fingerprint returns an error wrapping vector.ErrBuildingInProgress so
// the caller can surface an actionable message rather than a raw
// unique-index violation.
func (b *Backend) CreateGeneration(ctx context.Context, model string, dim int, fingerprint string) (vector.GenerationID, error) {
	if err := EnsureVectorTable(ctx, b.db, dim); err != nil {
		return 0, err
	}
	fp := fingerprint
	if fp == "" {
		// Defensive default: a missing fingerprint loses the staleness
		// signal that callers depend on. Fall back to the legacy
		// model:dim format rather than write an empty string into the
		// DB, but log nothing — tests intentionally pass an empty
		// fingerprint to exercise the old shape, and adding noise
		// there hides real failures.
		fp = fmt.Sprintf("%s:%d", model, dim)
	}
	now := time.Now().Unix()

	gen, _, err := b.claimOrInsertBuilding(ctx, model, dim, fp, now)
	if err != nil {
		return 0, err
	}
	return gen, nil
}

// claimOrInsertBuilding returns (id, isNew, err). isNew=true means
// this call inserted a fresh building row; isNew=false means we
// reused an existing building row whose fingerprint matched. Reusing
// an existing row keeps interrupted rebuilds idempotent.
//
// On a UNIQUE-constraint failure during INSERT (a concurrent caller
// raced us between SELECT and INSERT), we re-read the now-visible
// building row and return it instead of bubbling the raw SQLite
// error: this closes the read-then-insert gap that would otherwise
// surface as "UNIQUE constraint failed" instead of a clean resume or
// a wrapped ErrBuildingInProgress.
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

	// seeded_at is stamped at creation as harmless vestigial metadata:
	// scan-and-fill has no separate seed pass, and activation no longer
	// gates on it (coverage is the real gate). Kept only so the column is
	// populated for legacy display.
	res, err := b.db.ExecContext(ctx,
		`INSERT INTO index_generations
		 (model, dimension, fingerprint, started_at, seeded_at, state)
		 VALUES (?, ?, ?, ?, ?, 'building')`,
		model, dim, fp, now, now)
	if err != nil {
		// A concurrent CreateGeneration may have inserted between our
		// SELECT and INSERT. The unique partial index on (state) where
		// state='building' rejects the second writer. Re-read and
		// return the existing row (clean resume) or wrap
		// ErrBuildingInProgress (mismatched fingerprint).
		if isUniqueConstraintErr(err) {
			id, existingFP, ok, lookupErr := b.lookupBuilding(ctx)
			if lookupErr != nil {
				return 0, false, fmt.Errorf("lookup after insert race: %w", lookupErr)
			}
			if !ok {
				// The concurrent writer already activated/retired
				// before we could re-read. Surface the original
				// constraint failure rather than swallow it.
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
	newID, err := res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("new generation id: %w", err)
	}
	return vector.GenerationID(newID), true, nil
}

// lookupBuilding returns the current building generation's id and
// fingerprint. ok=false (with err=nil) means there is no building row.
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

// isUniqueConstraintErr reports whether err originates from SQLite's
// UNIQUE constraint enforcement, using the typed driver error code
// rather than message text so locale or version changes in the
// driver's error formatting don't silently break detection.
func isUniqueConstraintErr(err error) bool {
	var sqliteErr sqlite3.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	return sqliteErr.ExtendedCode == sqlite3.ErrConstraintUnique ||
		sqliteErr.Code == sqlite3.ErrConstraint &&
			sqliteErr.ExtendedCode == sqlite3.ErrConstraintPrimaryKey
}

// hasMissingForGen reports whether any live message in the main DB still
// needs embedding for gen (embed_gen IS NULL OR embed_gen <> gen). This is
// the scan-and-fill coverage gate. On SQLite the messages live in the main
// DB while the generation lifecycle lives in vectors.db, so the gate
// cannot be folded into the activation tx.
func (b *Backend) hasMissingForGen(ctx context.Context, gen vector.GenerationID) (bool, error) {
	var exists int
	where, args := b.missingCoverageWhere(int64(gen))
	err := b.mainDB.QueryRowContext(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM messages
			 WHERE `+where+`
		)`, args...).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check missing coverage for generation %d: %w", gen, err)
	}
	return exists == 1, nil
}

func (b *Backend) missingCoverageWhere(gen int64) (string, []any) {
	where := "(embed_gen IS NULL OR embed_gen <> ?) AND " + store.LiveMessagesWhere("", true)
	args := []any{gen}
	if !b.scope.IsEmpty() {
		placeholders := make([]string, len(b.scope.MessageTypes))
		for i, typ := range b.scope.MessageTypes {
			placeholders[i] = "?"
			args = append(args, typ)
		}
		where += fmt.Sprintf(" AND message_type IN (%s)", strings.Join(placeholders, ","))
	}
	return where, args
}

// ActivateGeneration atomically retires the current active generation
// (if any) and promotes `gen` to active.
func (b *Backend) ActivateGeneration(ctx context.Context, gen vector.GenerationID, force bool) error {
	// Lifecycle pre-check: verify gen exists AND is in 'building' state
	// BEFORE the coverage pre-check below. The coverage predicate
	// (embed_gen IS NULL OR embed_gen <> gen) is true for an unknown gen id,
	// so an unknown/non-building gen would otherwise surface the misleading
	// "messages needing embedding" coverage error instead of the real
	// lifecycle error. The vectors.db tx's gated UPDATE re-derives this
	// invariant atomically (via activateGateError); this read-only lookup
	// just orders the errors correctly. Force does not bypass it — a force
	// activation of an unknown/non-building gen is still a lifecycle error,
	// matching the tx's WHERE id = ? AND state = 'building' clause.
	var state vector.GenerationState
	if err := b.db.QueryRowContext(ctx,
		`SELECT state FROM index_generations WHERE id = ?`, int64(gen)).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %d", vector.ErrUnknownGeneration, gen)
		}
		return fmt.Errorf("lookup generation %d: %w", gen, err)
	}
	if state != vector.GenerationBuilding {
		return fmt.Errorf("generation %d not in 'building' state", gen)
	}

	// Coverage pre-check: refuse to activate a generation that still
	// has live messages needing embedding, unless force. Cross-DB on
	// SQLite, so it runs here as a Go pre-check before the vectors.db tx;
	// the backstop covers the TOCTOU window.
	if !force {
		missing, err := b.hasMissingForGen(ctx, gen)
		if err != nil {
			return err
		}
		if missing {
			return fmt.Errorf("generation %d still has messages needing embedding; run `msgvault embeddings resume` or pass --force", gen)
		}
	}

	now := time.Now().Unix()
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Demote the current active generation (if any). sqlitevec retains
	// retired generations' vectors (its vec0 PARTITION KEY isolates them),
	// so there is nothing else to reap. Done inside the tx so the demote is
	// atomic with the activation below.
	if _, err := tx.ExecContext(ctx,
		`UPDATE index_generations
		 SET state = 'retired', completed_at = COALESCE(completed_at, ?)
		 WHERE state = 'active'`, now); err != nil {
		return fmt.Errorf("retire previous active: %w", err)
	}
	// Promote gen to active. The coverage gate (no live message still
	// needs embedding for gen) was enforced by the Go pre-check above
	// against the main DB; here we only enforce the lifecycle invariant
	// (gen is in 'building' state). The seeded_at gate was removed: seeding
	// was the old queue-population phase, which scan-and-fill no longer has,
	// so a legacy/crashed gen with seeded_at=NULL but full coverage must be
	// activatable. Coverage (missing==0) is the real gate.
	res, err := tx.ExecContext(ctx,
		`UPDATE index_generations
		 SET state = 'active', activated_at = ?, completed_at = COALESCE(completed_at, ?)
		 WHERE id = ? AND state = 'building'`, now, now, int64(gen))
	if err != nil {
		return fmt.Errorf("activate: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return activateGateError(ctx, tx, gen)
	}
	return tx.Commit()
}

// activateGateError re-reads gen inside the activation tx to return a
// precise reason the gated promote affected zero rows: unknown
// generation or not in 'building' state. The coverage (missing) gate is
// handled by the Go pre-check in ActivateGeneration, so it is not
// re-derived here.
func activateGateError(ctx context.Context, tx *sql.Tx, gen vector.GenerationID) error {
	var state vector.GenerationState
	if err := tx.QueryRowContext(ctx,
		`SELECT state FROM index_generations WHERE id = ?`, int64(gen)).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %d", vector.ErrUnknownGeneration, gen)
		}
		return fmt.Errorf("lookup generation %d: %w", gen, err)
	}
	return fmt.Errorf("generation %d not in 'building' state", gen)
}

// RetireGeneration marks the given generation as retired (a state flip
// only). sqlitevec retains the retired generation's vectors (vec0 PARTITION
// KEY isolation), so there is no queue to reap.
//
// Unless force is true, the state-flip UPDATE refuses to retire a generation
// in state='active' (WHERE state != 'active'): if it affects zero rows the
// active guard tripped, so the tx rolls back returning ErrRefuseRetireActive
// leaving state unchanged. SQLite serializes writers, so the guard and
// flip are atomic once inside the tx — closing the CLI's pre-flight TOCTOU so
// a concurrent activation cannot retire the now-serving generation without
// --force-active. force retires unconditionally (operator override).
func (b *Backend) RetireGeneration(ctx context.Context, gen vector.GenerationID, force bool) error {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin retire tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The active-gen guard is the WHERE clause itself: when force is false we
	// only retire a generation that is NOT active, so a concurrent activation
	// that flipped gen to active before this statement leaves zero rows
	// affected and we bail out before reaping anything. force=true drops the
	// guard (? OR ... is always satisfiable).
	res, err := tx.ExecContext(ctx,
		`UPDATE index_generations SET state = 'retired'
		 WHERE id = ? AND (? OR state != 'active')`, int64(gen), force)
	if err != nil {
		return fmt.Errorf("retire generation %d: %w", gen, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return retireGateError(ctx, tx, gen, force)
	}
	// Scan-and-fill has no per-generation queue to reap; sqlitevec retains
	// the retired generation's vectors (vec0 PARTITION KEY isolation).
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit retire generation %d: %w", gen, err)
	}
	return nil
}

// retireGateError re-reads gen inside the retire tx to explain why the gated
// state flip affected zero rows: the generation is active (and force was not
// passed) or it does not exist. Mirrors activateGateError so the management
// command gets precise, actionable errors now that the guard lives in the
// backend.
func retireGateError(ctx context.Context, tx *sql.Tx, gen vector.GenerationID, force bool) error {
	var state vector.GenerationState
	if err := tx.QueryRowContext(ctx,
		`SELECT state FROM index_generations WHERE id = ?`, int64(gen)).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %d", vector.ErrUnknownGeneration, gen)
		}
		return fmt.Errorf("lookup generation %d: %w", gen, err)
	}
	if state == vector.GenerationActive && !force {
		return fmt.Errorf("%w: generation %d", vector.ErrRefuseRetireActive, gen)
	}
	// A non-active row always matches `state != 'active'`, so the gated UPDATE
	// would have affected it (a no-op flip still counts as a matched row).
	// Reaching here for a non-active, existing generation means the row
	// vanished mid-tx; surface it rather than reporting a phantom retire.
	return fmt.Errorf("retire generation %d: state flip affected no rows (state=%q)", gen, state)
}

// ActiveGeneration returns the current active generation, or
// vector.ErrNoActiveGeneration if none exists.
func (b *Backend) ActiveGeneration(ctx context.Context) (vector.Generation, error) {
	return b.generationByState(ctx, vector.GenerationActive)
}

// BuildingGeneration returns the current building generation, or nil if
// none exists.
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
		 FROM index_generations WHERE state = ?`, string(state)).Scan(
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

// Upsert writes chunks to the given generation. Transactional. Dimension
// is verified per-chunk against the generation's recorded dimension.
// Returns an error wrapping vector.ErrUnknownGeneration if gen does not
// exist, and an error wrapping vector.ErrDimensionMismatch if any chunk's
// vector length does not match the generation's recorded dimension.
//
// Upsert does NOT touch messages.embed_gen — that is the worker's
// responsibility (it stamps embed_gen AFTER a successful upsert, an
// ordered idempotent step since the two live in different DBs on SQLite).
func (b *Backend) Upsert(ctx context.Context, gen vector.GenerationID, chunks []vector.Chunk) error {
	if len(chunks) == 0 {
		return nil
	}

	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin upsert tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Read the generation's dimension and lifecycle state inside the write
	// transaction and refuse to write to a retired generation. SQLite has
	// no SELECT ... FOR UPDATE, but a write tx serializes against other
	// writers (Activate/Retire), so this read is consistent for the life of
	// the upsert. sqlitevec's vec0 PARTITION KEY isolates retired rows so it
	// does not delete them on retire, making re-pollution impossible here;
	// the guard is kept for symmetry with the pgvector backend and to
	// document the invariant that retired generations are immutable.
	var dim int
	var state string
	err = tx.QueryRowContext(ctx,
		`SELECT dimension, state FROM index_generations WHERE id = ?`, int64(gen)).Scan(&dim, &state)
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
			return fmt.Errorf("%w: chunk %d for msg %d has %d dims, gen has %d",
				vector.ErrDimensionMismatch, c.ChunkIndex, c.MessageID, len(c.Vector), dim)
		}
	}

	// message_count tracks distinct messages, not chunks. Count how
	// many of the message_ids in this batch already have any row in
	// the generation so we can apply an O(1) delta instead of
	// rescanning the table. The "distinct" semantics matter because a
	// single upsert may carry multiple chunks for the same message.
	distinctIDs := distinctMessageIDs(chunks)
	preexisting, err := countExistingMessages(ctx, tx, gen, distinctIDs)
	if err != nil {
		return err
	}

	now := time.Now().Unix()
	vecTable := VectorTableName(dim)

	// Idempotency: clear any prior rows for the message_ids we're
	// about to replace. Chunking is not stable across upserts (the
	// same message may have produced 3 chunks last time and 2 this
	// time, e.g. after a preprocess change), so partial replacement
	// would leave orphaned tail chunks behind. Delete from vec0 first
	// — it references embedding_id values that vanish from embeddings
	// next.
	if err := deleteForMessageIDs(ctx, tx, vecTable, gen, distinctIDs); err != nil {
		return err
	}

	embedInsertStmt, err := tx.PrepareContext(ctx, `INSERT INTO embeddings
		(generation_id, message_id, chunk_index, embedded_at,
		 source_char_len, chunk_char_start, chunk_char_end, truncated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING embedding_id`)
	if err != nil {
		return fmt.Errorf("prepare embeddings insert: %w", err)
	}
	defer func() { _ = embedInsertStmt.Close() }()

	// vecTable name comes from VectorTableName(dim) where dim is sourced from index_generations; safe to interpolate.
	vecStmt, err := tx.PrepareContext(ctx, fmt.Sprintf(
		`INSERT INTO %s (generation_id, embedding_id, embedding) VALUES (?, ?, ?)`, vecTable))
	if err != nil {
		return fmt.Errorf("prepare vec insert: %w", err)
	}
	defer func() { _ = vecStmt.Close() }()

	for _, c := range chunks {
		truncFlag := 0
		if c.Truncated {
			truncFlag = 1
		}
		var embeddingID int64
		if err := embedInsertStmt.QueryRowContext(ctx,
			int64(gen), c.MessageID, c.ChunkIndex, now,
			c.SourceCharLen, c.ChunkCharStart, c.ChunkCharEnd, truncFlag,
		).Scan(&embeddingID); err != nil {
			return fmt.Errorf("insert embedding (msg %d chunk %d): %w", c.MessageID, c.ChunkIndex, err)
		}
		if _, err := vecStmt.ExecContext(ctx, int64(gen), embeddingID, float32SliceBlob(c.Vector)); err != nil {
			return fmt.Errorf("insert vector (msg %d chunk %d): %w", c.MessageID, c.ChunkIndex, err)
		}
	}

	delta := len(distinctIDs) - preexisting
	if err := applyMessageCountDelta(ctx, tx, gen, delta); err != nil {
		return err
	}
	return tx.Commit()
}

// distinctMessageIDs returns the unique message_ids referenced by
// chunks, preserving first-seen order. Order is irrelevant for
// idempotency but stable iteration helps in tests.
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

// deleteForMessageIDs removes every chunk (in both vec0 and embeddings)
// belonging to the given message_ids under gen. The vec0 delete runs
// first so its rowids (which equal embeddings.embedding_id) still exist
// for the subquery to resolve. Used by Upsert for idempotent replace.
func deleteForMessageIDs(ctx context.Context, tx *sql.Tx, vecTable string, gen vector.GenerationID, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	blob, err := json.Marshal(ids)
	if err != nil {
		return fmt.Errorf("encode msg ids: %w", err)
	}
	// vec0 partition-aware filter (generation_id) so the engine can
	// prune by partition before scanning, then embedding_id IN (...).
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
		DELETE FROM %s WHERE generation_id = ? AND embedding_id IN (
			SELECT embedding_id FROM embeddings
			WHERE generation_id = ?
			  AND message_id IN (SELECT value FROM json_each(?))
		)`, vecTable), int64(gen), int64(gen), string(blob)); err != nil {
		return fmt.Errorf("delete vectors: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM embeddings
		WHERE generation_id = ?
		  AND message_id IN (SELECT value FROM json_each(?))`,
		int64(gen), string(blob)); err != nil {
		return fmt.Errorf("delete embeddings: %w", err)
	}
	return nil
}

// applyMessageCountDelta nudges index_generations.message_count by
// delta inside the caller's transaction. Used by Upsert and Delete to
// keep the generation metadata in sync without rescanning the whole
// embeddings table on every batch (a full COUNT(*) per Upsert turned
// large rebuilds quadratic). delta=0 is a no-op.
func applyMessageCountDelta(ctx context.Context, tx *sql.Tx, gen vector.GenerationID, delta int) error {
	if delta == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE index_generations SET message_count = message_count + ? WHERE id = ?`,
		delta, int64(gen))
	if err != nil {
		return fmt.Errorf("update message_count: %w", err)
	}
	return nil
}

// countExistingMessages returns how many of ids already have at least
// one row in embeddings for the given generation. Note "messages" not
// "embeddings": message_count tracks distinct messages, so a message
// with 5 chunks counts once. Used by Upsert to compute the O(1)
// message_count delta. ids is JSON-encoded and consumed via json_each
// so the bind-parameter count stays at 2 regardless of batch size.
func countExistingMessages(ctx context.Context, tx *sql.Tx, gen vector.GenerationID, ids []int64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	blob, err := json.Marshal(ids)
	if err != nil {
		return 0, fmt.Errorf("encode ids: %w", err)
	}
	var n int
	err = tx.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT message_id) FROM embeddings
		  WHERE generation_id = ?
		    AND message_id IN (SELECT value FROM json_each(?))`,
		int64(gen), string(blob)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count existing messages: %w", err)
	}
	return n, nil
}

// float32SliceBlob converts a float32 slice to the little-endian byte
// representation that sqlite-vec expects.
func float32SliceBlob(v []float32) []byte {
	buf := make([]byte, 4*len(v))
	for i, f := range v {
		bits := math.Float32bits(f)
		buf[4*i] = byte(bits & 0xff)
		buf[4*i+1] = byte((bits >> 8) & 0xff)
		buf[4*i+2] = byte((bits >> 16) & 0xff)
		buf[4*i+3] = byte(bits >> 24)
	}
	return buf
}

// blobToFloat32 decodes the little-endian byte representation produced
// by float32SliceBlob back into a float32 slice of length dim.
func blobToFloat32(b []byte, dim int) ([]float32, error) {
	if len(b) != 4*dim {
		return nil, fmt.Errorf("blob length %d does not match dimension %d", len(b), dim)
	}
	out := make([]float32, dim)
	for i := range dim {
		bits := uint32(b[4*i]) | uint32(b[4*i+1])<<8 | uint32(b[4*i+2])<<16 | uint32(b[4*i+3])<<24
		out[i] = math.Float32frombits(bits)
	}
	return out, nil
}

// LoadVector returns the embedding for a specific message in the active
// generation. Returns vector.ErrNoActiveGeneration if no active
// generation exists, or a descriptive error if the message is not
// embedded in the active generation.
func (b *Backend) LoadVector(ctx context.Context, messageID int64) ([]float32, error) {
	active, err := b.ActiveGeneration(ctx)
	if err != nil {
		return nil, err
	}
	// Return the chunk_index=0 vector — the head of the message,
	// which always exists for any embedded message regardless of how
	// many additional chunks it has. find_similar callers (the only
	// consumer of LoadVector today) want one representative vector;
	// they treat embeddings as message-level. vecTable name derives
	// from VectorTableName(active.Dimension) where dimension is sourced
	// from index_generations; safe to interpolate.
	q := fmt.Sprintf(`
		SELECT v.embedding
		  FROM %s v
		  JOIN embeddings e ON e.embedding_id = v.embedding_id
		 WHERE v.generation_id = ?
		   AND e.message_id = ?
		   AND e.chunk_index = 0`,
		VectorTableName(active.Dimension))
	var blob []byte
	err = b.db.QueryRowContext(ctx, q, int64(active.ID), messageID).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("no embedding for message %d in generation %d", messageID, active.ID)
	}
	if err != nil {
		return nil, fmt.Errorf("load vector for message %d: %w", messageID, err)
	}
	return blobToFloat32(blob, active.Dimension)
}

// ResetWatermarkBelow lowers the embed_watermark for EVERY generation to at
// most minID-1 (clamped at 0) so a subsequent incremental RunOnce re-scans
// from below minID and re-finds rows whose embed_gen was just reset to NULL
// by repair-encoding. The watermark lives in vectors.db on SQLite (b.db).
//
// SQLite's MIN(a, b) is the scalar two-argument minimum (not the aggregate),
// so `watermark_id = MIN(watermark_id, ?)` never raises a generation's
// cursor — it only lowers one that currently sits above the new floor. minID
// < 1 is a no-op (nothing below id 1). Idempotent: a second call with the
// same or higher minID changes nothing.
func (b *Backend) ResetWatermarkBelow(ctx context.Context, minID int64) error {
	if minID < 1 {
		return nil
	}
	floorID := minID - 1
	if _, err := b.db.ExecContext(ctx,
		`UPDATE embed_watermark SET watermark_id = MIN(watermark_id, ?)`, floorID); err != nil {
		return fmt.Errorf("reset watermark below %d: %w", minID, err)
	}
	return nil
}

// Search runs an ANN query against the given generation and returns the
// top-k hits (optionally intersected with a structured filter). Hits are
// ordered by ascending distance and assigned 1-based ranks.
func (b *Backend) Search(ctx context.Context, gen vector.GenerationID, queryVec []float32, k int, filter vector.Filter) ([]vector.Hit, error) {
	if len(queryVec) == 0 {
		return nil, errors.New("search: empty query vector")
	}

	var dim int
	err := b.db.QueryRowContext(ctx,
		`SELECT dimension FROM index_generations WHERE id = ?`, int64(gen)).Scan(&dim)
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
	vecTable := VectorTableName(dim)

	// Fast path: when no structured filter is set, run the ANN query
	// unconstrained and post-filter deletions against the small hit
	// set. This avoids fetching every live message ID from main.db
	// just to satisfy the deletion predicate, which is an O(total live
	// messages) cost on every pure-vector search or find_similar call.
	//
	// Soft-deleted rows that land in the top-k would shrink the
	// returned set below what the caller asked for. sqlite-vec doesn't
	// page, so we start with a 2× over-fetch and keep doubling up to
	// the generation's total embedded count when deletions turn out
	// to cluster more densely than the initial pass covered. The loop
	// always terminates: each iteration either satisfies k or grows
	// fetch toward the fixed ceiling.
	if filter.IsEmpty() {
		// chunkCeiling is the actual number of rows in vec0 for this
		// generation — the upper bound for the over-fetch loop. Using
		// message_count (distinct messages) instead would under-shoot
		// when avg_chunks_per_msg > chunkOverfetchFactor and could
		// short-return when soft-deletions cluster densely. Counting
		// embeddings rows uses the existing idx_embeddings_gen_msg
		// index — O(rows-for-gen) but in practice fast because
		// SQLite optimises COUNT(*) on a covered index.
		var chunkCeiling int
		if err := b.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM embeddings WHERE generation_id = ?`,
			int64(gen)).Scan(&chunkCeiling); err != nil {
			return nil, fmt.Errorf("lookup chunk count: %w", err)
		}
		if chunkCeiling == 0 {
			return nil, nil
		}
		// Group by message_id (each message may have multiple chunks);
		// MIN(distance) keeps the best-scoring chunk and discards the
		// rest. Order applies after the group, so the ranking is by
		// best-chunk distance per message.
		q := fmt.Sprintf(`
			SELECT e.message_id, MIN(v.distance) AS distance
			  FROM %s v
			  JOIN embeddings e ON e.embedding_id = v.embedding_id
			 WHERE v.generation_id = ?
			   AND v.embedding MATCH ?
			   AND k = ?
			 GROUP BY e.message_id
			 ORDER BY distance ASC
		`, vecTable)
		// Two over-fetch dimensions stacked:
		//   - chunk-level: with N chunks/msg, a top-k by chunk could
		//     pack the result with one or two messages' tail chunks.
		//     Multiply by chunkOverfetchFactor so the GROUP BY can
		//     still pick out k distinct messages.
		//   - deletion-level: existing soft-delete filter may shrink
		//     the result; the doubling loop below already handles
		//     this dimension.
		// max() guards against overflow or degenerate small k.
		fetch := max(k*chunkOverfetchFactor*deletedOverfetchFactor, k)
		for {
			if fetch > chunkCeiling {
				fetch = chunkCeiling
			}
			hits, err := b.scanHits(ctx, q, int64(gen), float32SliceBlob(queryVec), fetch)
			if err != nil {
				return nil, err
			}
			filtered, err := b.dropDeletedFromSource(ctx, hits)
			if err != nil {
				return nil, err
			}
			if len(filtered) >= k || fetch >= chunkCeiling {
				if len(filtered) > k {
					filtered = filtered[:k]
				}
				// Re-rank so callers see 1,2,3… rather than the sparse
				// ranks sqlite-vec assigned (deleted rows were at
				// intermediate positions).
				for i := range filtered {
					filtered[i].Rank = i + 1
				}
				return filtered, nil
			}
			fetch *= 2
		}
	}

	idClause, filterArgs, err := b.resolveFilter(ctx, filter)
	if err != nil {
		return nil, err
	}

	// The filtered path's GROUP BY hides the same hazard as the
	// empty-filter path: a few messages with many matching chunks
	// could pack the top-k chunk pool and collapse below k distinct
	// messages once grouped. Widen the chunk fetch with the same
	// doubling loop, bounded by the actual chunk count in the
	// generation so the loop always terminates.
	var chunkCeiling int
	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = ?`,
		int64(gen)).Scan(&chunkCeiling); err != nil {
		return nil, fmt.Errorf("lookup chunk count: %w", err)
	}
	if chunkCeiling == 0 {
		return nil, nil
	}

	// Filtered-message ceiling: count distinct filtered messages
	// that have at least one chunk in this generation. The widening
	// loop below uses this as an early exit so a selective filter
	// (few matching messages) doesn't drive the loop all the way to
	// the generation-wide chunkCeiling: once len(hits) reaches the
	// filtered-message count, every filtered message is in the
	// result set with its best-distance chunk (further iterations
	// only fetch chunks with worse distances, so neither the set of
	// messages nor each message's MIN(distance) can change).
	filteredMessageCeiling := chunkCeiling // empty-filter falls through to the fast path above; this clause runs only for non-empty filters
	ceilingSQL := `SELECT COUNT(DISTINCT message_id) FROM embeddings e WHERE e.generation_id = ? ` + idClause
	ceilingArgs := make([]any, 0, 1+len(filterArgs))
	ceilingArgs = append(ceilingArgs, int64(gen))
	ceilingArgs = append(ceilingArgs, filterArgs...)
	if err := b.db.QueryRowContext(ctx, ceilingSQL, ceilingArgs...).Scan(&filteredMessageCeiling); err != nil {
		return nil, fmt.Errorf("lookup filtered message count: %w", err)
	}
	if filteredMessageCeiling == 0 {
		return nil, nil
	}

	q := fmt.Sprintf(`
		SELECT e.message_id, MIN(v.distance) AS distance
		  FROM %s v
		  JOIN embeddings e ON e.embedding_id = v.embedding_id
		 WHERE v.generation_id = ?
		   AND v.embedding MATCH ?
		   AND k = ?
		   %s
		 GROUP BY e.message_id
		 ORDER BY distance ASC
	`, vecTable, idClause)

	fetch := max(k*chunkOverfetchFactor, k)
	for {
		if fetch > chunkCeiling {
			fetch = chunkCeiling
		}
		allArgs := make([]any, 0, 3+len(filterArgs))
		allArgs = append(allArgs, int64(gen), float32SliceBlob(queryVec), fetch)
		allArgs = append(allArgs, filterArgs...)

		hits, err := b.scanHits(ctx, q, allArgs...)
		if err != nil {
			return nil, err
		}
		// Exit when either: we have k distinct messages (caller is
		// satisfied), every filtered message has been found (no new
		// messages will appear regardless of further iterations —
		// see filteredMessageCeiling comment above), or we've
		// scanned the entire generation. Without the middle
		// condition, a selective filter that matches m < k messages
		// would drive the loop all the way to chunkCeiling doing
		// wasted ANN work; with it, the loop terminates as soon as
		// the filtered universe has been fully resolved.
		if len(hits) >= k || len(hits) >= filteredMessageCeiling || fetch >= chunkCeiling {
			if len(hits) > k {
				hits = hits[:k]
			}
			return hits, nil
		}
		fetch *= 2
	}
}

// chunkOverfetchFactor multiplies the requested k when fetching from
// the vec0 table so the message-level GROUP BY downstream still has
// enough chunks to surface k distinct messages. Most messages produce
// a single chunk; this factor only matters for the long tail. 4× is
// generous for typical email corpora (avg ~1.1 chunks/msg) and cheap
// — sqlite-vec returns the top-N rows in O(N log N) regardless.
const chunkOverfetchFactor = 4

// scanHits runs an ANN query and materializes hits in distance order
// (higher score = better). Extracted so Search can share the scan
// logic between its empty-filter fast path and the filtered path.
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

// resolveFilter returns a SQL fragment constraining the JOINed
// embeddings.message_id to the set of messages that pass the structured
// filter, along with the args to bind. For a populated filter this
// also enforces the deletion check inline via filteredMessageIDs;
// empty filters take the fast path in Search and post-filter deletions
// on the smaller hit set instead of materializing the entire corpus ID
// list here.
//
// The fragment uses json_each over a single JSON-encoded id list, so
// the bind-parameter count is O(1) no matter how many messages match
// — this keeps broad filters (one account, one common label, wide
// date range) under SQLite's ~999-parameter practical cap.
//
// The fragment qualifies `message_id` with the `e.` alias from Search's
// SELECT (`embeddings e JOIN vectors_vec_dN v ...`) so the column is
// unambiguous: the vec0 table itself no longer carries `message_id`
// post-chunking, but a stray reference here would have read as
// "embeddings.message_id" by accident, masking the chunking change.
func (b *Backend) resolveFilter(ctx context.Context, filter vector.Filter) (string, []any, error) {
	ids, err := b.filteredMessageIDs(ctx, filter)
	if err != nil {
		return "", nil, err
	}
	if len(ids) == 0 {
		return "AND e.message_id IN (SELECT NULL WHERE 0)", nil, nil
	}
	blob, err := json.Marshal(ids)
	if err != nil {
		return "", nil, fmt.Errorf("encode filter ids: %w", err)
	}
	return "AND e.message_id IN (SELECT value FROM json_each(?))", []any{string(blob)}, nil
}

// deletedOverfetchFactor is the starting multiplier applied to k on
// the empty-filter fast path to absorb soft-deleted rows that would
// otherwise shrink the returned set below the caller's requested
// count. When deletions cluster more densely than this first pass
// covers, Search keeps doubling fetch up to the generation's embedded
// count. 2× is a good opening bid: most archives have sparse deletions
// so the first pass succeeds, and the doubling fallback caps the worst
// case at O(embedded count) rather than leaving the caller short.
const deletedOverfetchFactor = 2

// dropDeletedFromSource takes ANN hits and returns the subset that
// are live messages (deleted_at IS NULL AND deleted_from_source_at IS NULL)
// in main.db, preserving the input order. Used by Search on the empty-
// filter fast path so that pure-vector/find_similar callers don't pay
// the cost of materializing the full live-corpus id list just to
// enforce the deletion check.
func (b *Backend) dropDeletedFromSource(ctx context.Context, hits []vector.Hit) ([]vector.Hit, error) {
	if len(hits) == 0 {
		return hits, nil
	}
	ids := make([]int64, len(hits))
	for i, h := range hits {
		ids[i] = h.MessageID
	}
	blob, err := json.Marshal(ids)
	if err != nil {
		return nil, fmt.Errorf("encode hit ids: %w", err)
	}
	q := `SELECT id FROM messages
	       WHERE id IN (SELECT value FROM json_each(?))
	         AND ` + store.LiveMessagesWhere("", true)
	rows, err := b.mainDB.QueryContext(ctx, q, string(blob))
	if err != nil {
		return nil, fmt.Errorf("live-hit filter: %w", err)
	}
	defer func() { _ = rows.Close() }()
	live := make(map[int64]struct{}, len(hits))
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan live id: %w", err)
		}
		live[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate live ids: %w", err)
	}
	out := hits[:0]
	for _, h := range hits {
		if _, ok := live[h.MessageID]; ok {
			out = append(out, h)
		}
	}
	return out, nil
}

// filteredMessageIDs runs the filter against the main DB and returns
// matching message IDs. See spec §5.3.
func (b *Backend) filteredMessageIDs(ctx context.Context, f vector.Filter) ([]int64, error) {
	clauses := []string{store.LiveMessagesWhere("m", true)}
	var args []any

	if len(f.SourceIDs) > 0 {
		clauses = append(clauses, inClause("m.source_id", f.SourceIDs))
		for _, id := range f.SourceIDs {
			args = append(args, id)
		}
	}
	if len(f.MessageTypes) > 0 {
		clauses = append(clauses, inStringClause("m.message_type", f.MessageTypes))
		for _, typ := range f.MessageTypes {
			args = append(args, typ)
		}
	}
	// Sender filters: one EXISTS per group, AND'd across groups so
	// repeated `from:` operators each become an independent
	// message-level requirement. Each group matches solely against
	// the message's `from` recipient rows — the same source the
	// SQLite FTS path uses (internal/store/api.go:327-336). Using a
	// single source keeps repeated `from:` tokens coherent: mixed
	// satisfaction (one token via sender_id, another via recipient
	// row) cannot create matches the SQLite path would not also
	// produce.
	for _, group := range f.SenderGroups {
		if len(group) == 0 {
			continue
		}
		inRecipient := inClause("mr.participant_id", group)
		clauses = append(clauses, fmt.Sprintf(
			`EXISTS (
				SELECT 1 FROM message_recipients mr
				 WHERE mr.message_id = m.id
				   AND mr.recipient_type = 'from'
				   AND %s
			)`, inRecipient))
		for _, id := range group {
			args = append(args, id)
		}
	}
	// Recipient filters: one EXISTS per group, matching participant_id.
	// Multiple groups for the same recipient type are AND'd so that
	// `to:alice to:bob` requires the message to have a `to` recipient
	// matching alice AND a `to` recipient matching bob.
	addRecipientGroups := func(recipientType string, groups [][]int64) {
		for _, ids := range groups {
			if len(ids) == 0 {
				continue
			}
			clauses = append(clauses, fmt.Sprintf(
				`EXISTS (
					SELECT 1 FROM message_recipients mr
					 WHERE mr.message_id = m.id
					   AND mr.recipient_type = '%s'
					   AND %s
				)`, recipientType, inClause("mr.participant_id", ids)))
			for _, id := range ids {
				args = append(args, id)
			}
		}
	}
	addRecipientGroups("to", f.ToGroups)
	addRecipientGroups("cc", f.CcGroups)
	addRecipientGroups("bcc", f.BccGroups)

	if f.HasAttachment != nil {
		clauses = append(clauses, "m.has_attachments = ?")
		args = append(args, *f.HasAttachment)
	}
	if f.After != nil {
		clauses = append(clauses, "m.sent_at >= ?")
		args = append(args, f.After.Format(sqliteDatetimeFormat))
	}
	if f.Before != nil {
		clauses = append(clauses, "m.sent_at < ?")
		args = append(args, f.Before.Format(sqliteDatetimeFormat))
	}
	if f.LargerThan != nil {
		clauses = append(clauses, "m.size_estimate > ?")
		args = append(args, *f.LargerThan)
	}
	if f.SmallerThan != nil {
		clauses = append(clauses, "m.size_estimate < ?")
		args = append(args, *f.SmallerThan)
	}
	for _, term := range f.SubjectSubstrings {
		clauses = append(clauses, `m.subject LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLikeSubject(term)+"%")
	}
	// Label filters: one EXISTS per group, AND'd across groups so that
	// `label:promo label:billing` requires both labels to be present.
	for _, ids := range f.LabelGroups {
		if len(ids) == 0 {
			continue
		}
		clauses = append(clauses, fmt.Sprintf(
			`EXISTS (SELECT 1 FROM message_labels ml WHERE ml.message_id = m.id AND %s)`,
			inClause("ml.label_id", ids)))
		for _, id := range ids {
			args = append(args, id)
		}
	}

	query := `SELECT m.id FROM messages m WHERE ` + strings.Join(clauses, " AND ")

	rows, err := b.mainDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("filter query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan filter id: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate filter ids: %w", err)
	}
	return out, nil
}

// inClause returns "col IN (?,?,?)" for len(ids) placeholders. Caller
// must append the ids to the args slice in the same order.
func inClause(col string, ids []int64) string {
	placeholders := make([]string, len(ids))
	for i := range ids {
		placeholders[i] = "?"
	}
	return fmt.Sprintf("%s IN (%s)", col, strings.Join(placeholders, ","))
}

// inStringClause returns "col IN (?,?,?)" for string values. Caller must
// append the values to the args slice in the same order.
func inStringClause(col string, values []string) string {
	placeholders := make([]string, len(values))
	for i := range values {
		placeholders[i] = "?"
	}
	return fmt.Sprintf("%s IN (%s)", col, strings.Join(placeholders, ","))
}

// escapeLikeSubject escapes SQL LIKE special characters (%, _, \) so
// they match literally. Used with ESCAPE '\' to preserve semantics
// from the existing subject filter in internal/store/api.go.
func escapeLikeSubject(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// Delete removes the given messages from the specified generation in
// one transaction. Empty messageIDs is a no-op. Returns an error
// wrapping vector.ErrUnknownGeneration if gen does not exist.
func (b *Backend) Delete(ctx context.Context, gen vector.GenerationID, messageIDs []int64) error {
	if len(messageIDs) == 0 {
		return nil
	}

	var dim int
	err := b.db.QueryRowContext(ctx,
		`SELECT dimension FROM index_generations WHERE id = ?`, int64(gen)).Scan(&dim)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %d", vector.ErrUnknownGeneration, gen)
	}
	if err != nil {
		return fmt.Errorf("lookup generation %d: %w", gen, err)
	}

	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Count distinct messages that will actually be removed before
	// issuing the deletes so we can apply a precise message_count
	// delta. Counting up-front keeps the helper symmetric with the
	// Upsert path and works correctly for multi-chunk messages — one
	// message contributes one to message_count regardless of how many
	// chunks it has.
	willDelete, err := countExistingMessages(ctx, tx, gen, messageIDs)
	if err != nil {
		return err
	}

	// deleteForMessageIDs handles vec0 (via the embedding_id subquery)
	// and embeddings together. Both the vec0 and embeddings deletes
	// take a single JSON-each batch rather than per-id statements, so
	// the call count stays at 2 even for a large messageIDs list.
	if err := deleteForMessageIDs(ctx, tx, VectorTableName(dim), gen, messageIDs); err != nil {
		return err
	}
	if err := applyMessageCountDelta(ctx, tx, gen, -willDelete); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete tx: %w", err)
	}
	return nil
}

// Stats returns counts for the given generation. When gen == 0, counts
// are aggregated across all generations. Returns an error wrapping
// vector.ErrUnknownGeneration if gen != 0 and the generation does not
// exist, so callers can distinguish a bad gen id from a valid-but-empty
// generation. StorageBytes is left zero here; it is derived from the
// vectors.db file size by the caller.
func (b *Backend) Stats(ctx context.Context, gen vector.GenerationID) (vector.Stats, error) {
	var s vector.Stats
	where := "WHERE generation_id = ?"
	args := []any{int64(gen)}
	if gen == 0 {
		where, args = "", nil
	} else {
		var exists int
		err := b.db.QueryRowContext(ctx,
			`SELECT 1 FROM index_generations WHERE id = ?`, int64(gen)).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return s, fmt.Errorf("%w: %d", vector.ErrUnknownGeneration, gen)
		}
		if err != nil {
			return s, fmt.Errorf("lookup generation %d: %w", gen, err)
		}
	}

	// Count distinct messages, not rows. After chunking each long
	// message occupies multiple rows in the embeddings table, but the
	// "EmbeddingCount" semantic across the codebase (progress bar
	// denominator, generation summary, etc.) is "how many messages are
	// embedded" — counting rows would inflate by avg_chunks_per_msg
	// and break Done/Total invariants in internal/vector/stats.go.
	//
	// The aggregate path (gen == 0) counts DISTINCT (generation_id,
	// message_id) pairs rather than DISTINCT message_id alone: a
	// message that lives in both the active and a building generation
	// represents two units of embedded work, and the aggregate Stats
	// view exists precisely so operators can see total work done
	// across generations. Per-generation paths are already constrained
	// by the WHERE clause, so DISTINCT message_id there has no
	// undercount hazard.
	embeddingCountSQL := `SELECT COUNT(DISTINCT message_id) FROM embeddings ` + where
	if gen == 0 {
		embeddingCountSQL = `SELECT COUNT(*) FROM (SELECT DISTINCT generation_id, message_id FROM embeddings)`
	}
	if err := b.db.QueryRowContext(ctx, embeddingCountSQL, args...).Scan(&s.EmbeddingCount); err != nil {
		return s, fmt.Errorf("count embeddings: %w", err)
	}
	// PendingCount is now "messages still needing embedding for this
	// generation" (embed_gen <> gen), read from the main DB rather than a
	// queue table. The aggregate path (gen == 0) has no single target
	// generation, so it reports 0 — the StatsView consumer sums per-gen
	// pending across the active/building generations anyway. A nil mainDB
	// (e.g. management commands that open the backend without the main
	// handle) reports 0 rather than failing Stats.
	if gen != 0 && b.mainDB != nil {
		missingWhere, missingArgs := b.missingCoverageWhere(int64(gen))
		if err := b.mainDB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM messages WHERE `+missingWhere,
			missingArgs...).Scan(&s.PendingCount); err != nil {
			return s, fmt.Errorf("count missing: %w", err)
		}
	}
	return s, nil
}

// EmbeddedMessageCount returns the number of in-scope LIVE messages that
// are stamped for gen (embed_gen = gen) AND actually have at least one
// vector for the generation. Used by the coverage readout to split stamped
// messages into embedded vs blank. Counts distinct messages (not chunk
// rows) so a long, multi-chunk message counts once, matching the
// EmbeddingCount semantic elsewhere.
//
// The liveness + stamped filter is REQUIRED for the coverage invariant
// live == embedded + blank + missing to hold. A non-live message
// (soft-deleted via deleted_at / deleted_from_source_at, or a dedup
// loser) keeps its embedding rows — Backend.Delete has no production
// callers — so an unfiltered COUNT(DISTINCT message_id) over the
// embeddings table can exceed stamped (which is live-only), driving
// blank = stamped - embedded negative (clamped to 0) and breaking the
// invariant (EMBEDDED could display larger than LIVE).
//
// Cross-DB on SQLite: embeddings live in vectors.db (b.db) while messages
// + embed_gen live in main.db (b.mainDB), two separate *sql.DB handles, so
// this cannot be a single JOIN. ATTACH is not used because it does not
// persist reliably across database/sql pooled connections. Instead we
// mirror the established cross-DB pattern (see dropDeletedFromSource):
// pull the distinct embedded message ids from vectors.db, then intersect
// them against the live+stamped set in main.db via json_each. A nil
// mainDB (management commands that opened the backend without the main
// handle) falls back to the unfiltered vectors.db count.
func (b *Backend) EmbeddedMessageCount(ctx context.Context, gen vector.GenerationID) (int64, error) {
	if b.mainDB == nil {
		var n int64
		if err := b.db.QueryRowContext(ctx,
			`SELECT COUNT(DISTINCT message_id) FROM embeddings WHERE generation_id = ?`,
			int64(gen)).Scan(&n); err != nil {
			return 0, fmt.Errorf("count embedded messages: %w", err)
		}
		return n, nil
	}

	// Step 1 (vectors.db): distinct message ids with >=1 vector for gen.
	rows, err := b.db.QueryContext(ctx,
		`SELECT DISTINCT message_id FROM embeddings WHERE generation_id = ?`,
		int64(gen))
	if err != nil {
		return 0, fmt.Errorf("list embedded message ids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("scan embedded message id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate embedded message ids: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}

	// Step 2 (main.db): how many of those are in-scope, live, AND stamped for gen.
	blob, err := json.Marshal(ids)
	if err != nil {
		return 0, fmt.Errorf("encode embedded ids: %w", err)
	}
	where := `id IN (SELECT value FROM json_each(?))
		    AND embed_gen = ?
		    AND ` + store.LiveMessagesWhere("", true)
	args := []any{string(blob), int64(gen)}
	if !b.scope.IsEmpty() {
		placeholders := make([]string, len(b.scope.MessageTypes))
		for i, typ := range b.scope.MessageTypes {
			placeholders[i] = "?"
			args = append(args, typ)
		}
		where += fmt.Sprintf(" AND message_type IN (%s)", strings.Join(placeholders, ","))
	}
	var n int64
	if err := b.mainDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE `+where,
		args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count live embedded messages: %w", err)
	}
	return n, nil
}

// ScoreMessageChunks scores every embedded chunk of messageID in gen
// against queryVec. Results are sorted by score descending (best first).
func (b *Backend) ScoreMessageChunks(ctx context.Context, gen vector.GenerationID, messageID int64, queryVec []float32) ([]vector.ChunkHit, error) {
	if len(queryVec) == 0 {
		return nil, errors.New("score message chunks: empty query vector")
	}

	var dim int
	err := b.db.QueryRowContext(ctx,
		`SELECT dimension FROM index_generations WHERE id = ?`, int64(gen)).Scan(&dim)
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
	vecTable := VectorTableName(dim)

	q := fmt.Sprintf(`
		SELECT e.chunk_index, e.chunk_char_start, e.chunk_char_end, v.embedding
		  FROM embeddings e
		  JOIN %s v ON v.embedding_id = e.embedding_id
		 WHERE v.generation_id = ?
		   AND e.message_id = ?
		 ORDER BY e.chunk_index ASC`, vecTable)

	rows, err := b.db.QueryContext(ctx, q, int64(gen), messageID)
	if err != nil {
		return nil, fmt.Errorf("load message chunks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var hits []vector.ChunkHit
	for rows.Next() {
		var idx, start, end int
		var blob []byte
		if err := rows.Scan(&idx, &start, &end, &blob); err != nil {
			return nil, fmt.Errorf("scan chunk row: %w", err)
		}
		vec, err := blobToFloat32(blob, dim)
		if err != nil {
			return nil, fmt.Errorf("decode chunk %d vector: %w", idx, err)
		}
		dist := l2Distance(vec, queryVec)
		hits = append(hits, vector.ChunkHit{
			ChunkIndex:     idx,
			ChunkCharStart: start,
			ChunkCharEnd:   end,
			Score:          1.0 - dist,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate chunk rows: %w", err)
	}

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].ChunkIndex < hits[j].ChunkIndex
	})
	return hits, nil
}

// l2Distance returns the Euclidean distance between two equal-length
// vectors. sqlite-vec's MATCH distance uses L2, so this keeps
// within-message chunk scores aligned with corpus search scores.
func l2Distance(a, b []float32) float64 {
	var sum float64
	for i := range a {
		d := float64(a[i] - b[i])
		sum += d * d
	}
	return math.Sqrt(sum)
}
