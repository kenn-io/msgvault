//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
	"github.com/wesm/msgvault/internal/store"
	"github.com/wesm/msgvault/internal/vector"
)

// sqliteDatetimeFormat is the text DATETIME layout used everywhere
// else in the repository (see internal/query/sqlite.go). Bind date
// bounds with this format so boundary comparisons are consistent
// with the existing query paths.
const sqliteDatetimeFormat = "2006-01-02 15:04:05"

// Compile-time check that *Backend satisfies the vector.Backend interface.
var _ vector.Backend = (*Backend)(nil)

// Options configures how Open establishes a Backend.
type Options struct {
	Path      string
	MainPath  string  // filesystem path to msgvault.db; required for FusedSearch
	Dimension int     // default dimension for EnsureVectorTable at open
	MainDB    *sql.DB // handle to the main msgvault.db
}

// Backend implements vector.Backend and vector.FusingBackend against a
// SQLite database with the sqlite-vec extension.
type Backend struct {
	db       *sql.DB // handle to vectors.db
	mainDB   *sql.DB // handle to msgvault.db
	path     string  // filesystem path to vectors.db
	mainPath string  // filesystem path to msgvault.db (for ATTACH)
	dim      int
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
	return &Backend{
		db:       db,
		mainDB:   opts.MainDB,
		path:     opts.Path,
		mainPath: opts.MainPath,
		dim:      opts.Dimension,
	}, nil
}

// Close releases the vectors.db handle.
func (b *Backend) Close() error { return b.db.Close() }

// DB returns the underlying *sql.DB for vectors.db. Exposed for callers
// that need to share the pool (e.g. the embed worker's VectorsDB field).
func (b *Backend) DB() *sql.DB { return b.db }

// Path returns the filesystem path of vectors.db.
func (b *Backend) Path() string { return b.path }

// CreateGeneration allocates a new building generation (§5.1 of the
// spec) and seeds pending_embeddings with every currently-embeddable
// message. If a building generation already exists with the same
// fingerprint, returns its id so a crashed or interrupted rebuild can
// resume; on resume the seed pass is skipped iff the previous attempt
// recorded `seeded_at` (so messages that the previous attempt already
// embedded — and Queue.Complete therefore already removed from
// pending_embeddings — are NOT re-enqueued). When the previous attempt
// crashed BEFORE the seed transaction committed, seeded_at is NULL
// and we re-run seedPending so we don't activate an empty generation.
// seedPending uses INSERT OR IGNORE, so rerunning it is safe regardless
// of what the Enqueuer has dual-enqueued in the meantime.
//
// A mismatched fingerprint returns an error wrapping
// vector.ErrBuildingInProgress so the caller can surface an actionable
// message rather than a raw unique-index violation.
//
// Concurrency note: the new building generation is committed *before*
// seeding so that a concurrent Enqueuer (driven by sync) immediately
// sees the new generation and dual-enqueues newly-synced messages. The
// seed loop then uses INSERT OR IGNORE, so any rows the Enqueuer has
// already written are silently de-duplicated and nothing is missed.
func (b *Backend) CreateGeneration(ctx context.Context, model string, dim int) (vector.GenerationID, error) {
	if err := EnsureVectorTable(ctx, b.db, dim); err != nil {
		return 0, err
	}
	fp := fmt.Sprintf("%s:%d", model, dim)
	now := time.Now().Unix()

	gen, isNew, err := b.claimOrInsertBuilding(ctx, model, dim, fp, now)
	if err != nil {
		return 0, err
	}

	if !isNew {
		// Resume path: only skip seedPending when the prior attempt's
		// seed transaction committed. seeded_at IS NULL means the
		// process died between the building-row insert and the seed
		// commit; pending_embeddings is empty (or only contains
		// dual-enqueued rows from concurrent Enqueuer activity), so
		// activating now would silently replace a valid active index
		// with an unseeded one. Re-run seedPending; the INSERT OR
		// IGNORE statements de-duplicate against any dual-enqueued or
		// already-completed rows.
		seeded, err := b.isGenerationSeeded(ctx, gen)
		if err != nil {
			return 0, err
		}
		if seeded {
			return gen, nil
		}
		// Fall through to seedPending + mark seeded.
	}
	if err := b.seedPending(ctx, gen, now); err != nil {
		return 0, err
	}
	if err := b.markGenerationSeeded(ctx, gen, now); err != nil {
		return 0, err
	}
	return gen, nil
}

// isGenerationSeeded reports whether the initial seedPending pass for
// gen committed (seeded_at IS NOT NULL).
func (b *Backend) isGenerationSeeded(ctx context.Context, gen vector.GenerationID) (bool, error) {
	var seededAt sql.NullInt64
	err := b.db.QueryRowContext(ctx,
		`SELECT seeded_at FROM index_generations WHERE id = ?`, int64(gen)).Scan(&seededAt)
	if err != nil {
		return false, fmt.Errorf("read seeded_at: %w", err)
	}
	return seededAt.Valid, nil
}

// markGenerationSeeded stamps seeded_at on gen so future resume calls
// know the initial seed pass committed. Idempotent: COALESCE preserves
// the original timestamp when called more than once.
func (b *Backend) markGenerationSeeded(ctx context.Context, gen vector.GenerationID, now int64) error {
	if _, err := b.db.ExecContext(ctx,
		`UPDATE index_generations SET seeded_at = COALESCE(seeded_at, ?) WHERE id = ?`,
		now, int64(gen)); err != nil {
		return fmt.Errorf("mark generation seeded: %w", err)
	}
	return nil
}

// EnsureSeeded re-runs the initial seed pass for gen if seeded_at is
// NULL. Used on the resume path so that a crash between
// claimOrInsertBuilding and the original seedPending commit cannot
// cause a later `msgvault build-embeddings` to "drain" zero pending rows and
// activate an unseeded generation. Returns an error if gen no longer
// exists or has been activated/retired (state != 'building'); the
// caller should surface --full-rebuild guidance in that case.
func (b *Backend) EnsureSeeded(ctx context.Context, gen vector.GenerationID) error {
	var state string
	err := b.db.QueryRowContext(ctx,
		`SELECT state FROM index_generations WHERE id = ?`, int64(gen)).Scan(&state)
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

	res, err := b.db.ExecContext(ctx,
		`INSERT INTO index_generations
		 (model, dimension, fingerprint, started_at, state)
		 VALUES (?, ?, ?, ?, 'building')`,
		model, dim, fp, now)
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

// seedPending inserts one pending_embeddings row per non-deleted
// message in the main DB. Uses INSERT OR IGNORE so rows that the
// Enqueuer already added for this generation (via the dual-enqueue
// path) are silently de-duplicated, and the operation is safe to
// retry if interrupted. Runs under a single vectors.db transaction so
// the seed itself is atomic.
func (b *Backend) seedPending(ctx context.Context, gen vector.GenerationID, now int64) error {
	// Embedding-seeding: skip dedup-hidden and remote-deleted rows
	// using the canonical live-message predicate
	// (store.LiveMessagesWhere). Dedup Execute does not remove
	// vector-store rows by design: if a message is embedded then later
	// soft-deleted, the embedding stays in the vector store and
	// query-time live filtering (dropDeletedFromSource,
	// filteredMessageIDs) enforces the live-message contract.
	rows, err := b.mainDB.QueryContext(ctx,
		fmt.Sprintf(`SELECT id FROM messages WHERE %s`, store.LiveMessagesWhere("", true)))
	if err != nil {
		return fmt.Errorf("select messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin seed tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT OR IGNORE INTO pending_embeddings (generation_id, message_id, enqueued_at)
		 VALUES (?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare pending insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx, int64(gen), id, now); err != nil {
			return fmt.Errorf("insert pending: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return tx.Commit()
}

// ActivateGeneration atomically retires the current active generation
// (if any) and promotes `gen` to active.
func (b *Backend) ActivateGeneration(ctx context.Context, gen vector.GenerationID) error {
	now := time.Now().Unix()
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`UPDATE index_generations
		 SET state = 'retired', completed_at = COALESCE(completed_at, ?)
		 WHERE state = 'active'`, now); err != nil {
		return fmt.Errorf("retire previous active: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE index_generations
		 SET state = 'active', activated_at = ?, completed_at = COALESCE(completed_at, ?)
		 WHERE id = ? AND state = 'building'`, now, now, int64(gen))
	if err != nil {
		return fmt.Errorf("activate: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("generation %d not in 'building' state", gen)
	}
	return tx.Commit()
}

// RetireGeneration marks the given generation as retired.
func (b *Backend) RetireGeneration(ctx context.Context, gen vector.GenerationID) error {
	_, err := b.db.ExecContext(ctx,
		`UPDATE index_generations SET state = 'retired' WHERE id = ?`, int64(gen))
	return err
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
		return nil, nil
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
// Upsert does NOT touch pending_embeddings — that is the queue's
// responsibility and must go through Queue.Complete, which matches the
// claim_token so a late-finishing stale worker cannot erase the queue
// row belonging to the newer worker that has already reclaimed it.
func (b *Backend) Upsert(ctx context.Context, gen vector.GenerationID, chunks []vector.Chunk) error {
	if len(chunks) == 0 {
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
	for _, c := range chunks {
		if len(c.Vector) != dim {
			return fmt.Errorf("%w: chunk for msg %d has %d dims, gen has %d",
				vector.ErrDimensionMismatch, c.MessageID, len(c.Vector), dim)
		}
	}

	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin upsert tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Count how many of these message_ids already have a row in
	// embeddings for this generation, so we can apply an O(1) delta to
	// index_generations.message_count instead of rescanning the whole
	// table after every Upsert. Touches len(chunks) rows via the PK
	// index, not the entire generation.
	chunkIDs := make([]int64, len(chunks))
	for i, c := range chunks {
		chunkIDs[i] = c.MessageID
	}
	preexisting, err := countExistingEmbeddings(ctx, tx, gen, chunkIDs)
	if err != nil {
		return err
	}

	now := time.Now().Unix()
	vecTable := VectorTableName(dim)

	embedStmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO embeddings
		(generation_id, message_id, embedded_at, source_char_len, truncated)
		VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare embeddings insert: %w", err)
	}
	defer func() { _ = embedStmt.Close() }()

	// vecTable name comes from VectorTableName(dim) where dim is sourced from index_generations; safe to interpolate.
	vecDeleteStmt, err := tx.PrepareContext(ctx, fmt.Sprintf(
		`DELETE FROM %s WHERE generation_id = ? AND message_id = ?`, vecTable))
	if err != nil {
		return fmt.Errorf("prepare vec delete: %w", err)
	}
	defer func() { _ = vecDeleteStmt.Close() }()

	vecStmt, err := tx.PrepareContext(ctx, fmt.Sprintf(
		`INSERT INTO %s (generation_id, message_id, embedding) VALUES (?, ?, ?)`, vecTable))
	if err != nil {
		return fmt.Errorf("prepare vec insert: %w", err)
	}
	defer func() { _ = vecStmt.Close() }()

	for _, c := range chunks {
		truncFlag := 0
		if c.Truncated {
			truncFlag = 1
		}
		if _, err := embedStmt.ExecContext(ctx, int64(gen), c.MessageID, now, c.SourceCharLen, truncFlag); err != nil {
			return fmt.Errorf("insert embedding: %w", err)
		}
		// vec0 virtual tables do not support INSERT OR REPLACE for updates,
		// so delete any existing row first, then insert.
		if _, err := vecDeleteStmt.ExecContext(ctx, int64(gen), c.MessageID); err != nil {
			return fmt.Errorf("delete existing vector: %w", err)
		}
		if _, err := vecStmt.ExecContext(ctx, int64(gen), c.MessageID, float32SliceBlob(c.Vector)); err != nil {
			return fmt.Errorf("insert vector: %w", err)
		}
	}

	delta := len(chunks) - preexisting
	if err := applyMessageCountDelta(ctx, tx, gen, delta); err != nil {
		return err
	}
	return tx.Commit()
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

// countExistingEmbeddings returns how many of ids already have a row
// in embeddings for the given generation. The query touches len(ids)
// rows via the (generation_id, message_id) PK index, not the whole
// generation, so callers can compute an O(1) message_count delta. ids
// is JSON-encoded and consumed via json_each so the bind-parameter
// count stays at 2 regardless of batch size (matches the pattern used
// by resolveFilter for the same reason).
func countExistingEmbeddings(ctx context.Context, tx *sql.Tx, gen vector.GenerationID, ids []int64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	blob, err := json.Marshal(ids)
	if err != nil {
		return 0, fmt.Errorf("encode ids: %w", err)
	}
	var n int
	err = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings
		  WHERE generation_id = ?
		    AND message_id IN (SELECT value FROM json_each(?))`,
		int64(gen), string(blob)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count existing embeddings: %w", err)
	}
	return n, nil
}

// float32SliceBlob converts a float32 slice to the little-endian byte
// representation that sqlite-vec expects.
func float32SliceBlob(v []float32) []byte {
	buf := make([]byte, 4*len(v))
	for i, f := range v {
		bits := math.Float32bits(f)
		buf[4*i] = byte(bits)
		buf[4*i+1] = byte(bits >> 8)
		buf[4*i+2] = byte(bits >> 16)
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
	for i := 0; i < dim; i++ {
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
	// vecTable name derives from VectorTableName(active.Dimension) where dimension is sourced from index_generations; safe to interpolate.
	q := fmt.Sprintf(
		`SELECT embedding FROM %s WHERE generation_id = ? AND message_id = ?`,
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

// Search runs an ANN query against the given generation and returns the
// top-k hits (optionally intersected with a structured filter). Hits are
// ordered by ascending distance and assigned 1-based ranks.
func (b *Backend) Search(ctx context.Context, gen vector.GenerationID, queryVec []float32, k int, filter vector.Filter) ([]vector.Hit, error) {
	if len(queryVec) == 0 {
		return nil, fmt.Errorf("search: empty query vector")
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
		var embeddedCount int
		if err := b.db.QueryRowContext(ctx,
			`SELECT message_count FROM index_generations WHERE id = ?`,
			int64(gen)).Scan(&embeddedCount); err != nil {
			return nil, fmt.Errorf("lookup message count: %w", err)
		}
		if embeddedCount == 0 {
			return nil, nil
		}
		q := fmt.Sprintf(`
			SELECT message_id, distance
			  FROM %s
			 WHERE generation_id = ?
			   AND embedding MATCH ?
			   AND k = ?
			 ORDER BY distance ASC
		`, vecTable)
		fetch := k * deletedOverfetchFactor
		if fetch < k {
			fetch = k // guard against overflow or degenerate small k
		}
		for {
			if fetch > embeddedCount {
				fetch = embeddedCount
			}
			hits, err := b.scanHits(ctx, q, int64(gen), float32SliceBlob(queryVec), fetch)
			if err != nil {
				return nil, err
			}
			filtered, err := b.dropDeletedFromSource(ctx, hits)
			if err != nil {
				return nil, err
			}
			if len(filtered) >= k || fetch >= embeddedCount {
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

	q := fmt.Sprintf(`
		SELECT message_id, distance
		  FROM %s
		 WHERE generation_id = ?
		   AND embedding MATCH ?
		   AND k = ?
		   %s
		 ORDER BY distance ASC
	`, vecTable, idClause)

	allArgs := make([]any, 0, 3+len(filterArgs))
	allArgs = append(allArgs, int64(gen), float32SliceBlob(queryVec), k)
	allArgs = append(allArgs, filterArgs...)

	return b.scanHits(ctx, q, allArgs...)
}

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

// resolveFilter returns a SQL fragment constraining message_id to the
// set of messages that pass the structured filter, along with the args
// to bind. For a populated filter this also enforces the deletion check
// inline via filteredMessageIDs; empty filters take the fast path in
// Search and post-filter deletions on the smaller hit set instead of
// materializing the entire corpus ID list here.
//
// The fragment uses json_each over a single JSON-encoded id list, so
// the bind-parameter count is O(1) no matter how many messages match
// — this keeps broad filters (one account, one common label, wide
// date range) under SQLite's ~999-parameter practical cap.
func (b *Backend) resolveFilter(ctx context.Context, filter vector.Filter) (string, []any, error) {
	ids, err := b.filteredMessageIDs(ctx, filter)
	if err != nil {
		return "", nil, err
	}
	if len(ids) == 0 {
		return "AND message_id IN (SELECT NULL WHERE 0)", nil, nil
	}
	blob, err := json.Marshal(ids)
	if err != nil {
		return "", nil, fmt.Errorf("encode filter ids: %w", err)
	}
	return "AND message_id IN (SELECT value FROM json_each(?))", []any{string(blob)}, nil
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
	q := fmt.Sprintf(`SELECT id FROM messages
	       WHERE id IN (SELECT value FROM json_each(?))
	         AND %s`, store.LiveMessagesWhere("", true))
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

	// Count rows that will actually be removed before issuing the
	// per-id deletes so we can apply a precise message_count delta.
	// Counting up-front (rather than summing RowsAffected from each
	// per-id stmt) keeps the helper symmetric with the Upsert path
	// and avoids a second pass.
	willDelete, err := countExistingEmbeddings(ctx, tx, gen, messageIDs)
	if err != nil {
		return err
	}

	embedStmt, err := tx.PrepareContext(ctx,
		`DELETE FROM embeddings WHERE generation_id = ? AND message_id = ?`)
	if err != nil {
		return fmt.Errorf("prepare embeddings delete: %w", err)
	}
	defer func() { _ = embedStmt.Close() }()

	// vecTable name derives from VectorTableName(dim) where dim is sourced from index_generations; safe to interpolate.
	vecStmt, err := tx.PrepareContext(ctx, fmt.Sprintf(
		`DELETE FROM %s WHERE generation_id = ? AND message_id = ?`, VectorTableName(dim)))
	if err != nil {
		return fmt.Errorf("prepare vec delete: %w", err)
	}
	defer func() { _ = vecStmt.Close() }()

	for _, id := range messageIDs {
		if _, err := embedStmt.ExecContext(ctx, int64(gen), id); err != nil {
			return fmt.Errorf("delete embedding: %w", err)
		}
		if _, err := vecStmt.ExecContext(ctx, int64(gen), id); err != nil {
			return fmt.Errorf("delete vector: %w", err)
		}
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

	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings `+where, args...).Scan(&s.EmbeddingCount); err != nil {
		return s, fmt.Errorf("count embeddings: %w", err)
	}
	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings `+where, args...).Scan(&s.PendingCount); err != nil {
		return s, fmt.Errorf("count pending: %w", err)
	}
	return s, nil
}
