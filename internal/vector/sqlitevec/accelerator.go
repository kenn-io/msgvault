//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec/vec1"
)

const acceleratorKind = "vec1_ivf_opq"

// AcceleratorState is the lifecycle state of one persisted SQLite ANN index.
type AcceleratorState string

const (
	AcceleratorBuilding AcceleratorState = "building"
	AcceleratorReady    AcceleratorState = "ready"
	AcceleratorStale    AcceleratorState = "stale"
)

// AcceleratorStatus is bounded metadata about one generation's accelerator.
type AcceleratorStatus struct {
	GenerationID    vector.GenerationID
	State           AcceleratorState
	TableName       string
	Dimension       int
	IndexedCount    int64
	LastEmbeddingID int64
	SourceRevision  int64
	ModelConfig     string
	Vec1Version     string
	StartedAt       time.Time
	CompletedAt     *time.Time
	LastError       string
}

// Accelerator returns accelerator metadata for generationID. A nil status
// means the generation has not been optimized.
func (b *Backend) Accelerator(ctx context.Context, generationID vector.GenerationID) (*AcceleratorStatus, error) {
	var a AcceleratorStatus
	var startedAt int64
	var completedAt sql.NullInt64
	var lastError sql.NullString
	err := b.db.QueryRowContext(ctx, `
		SELECT generation_id, state, table_name, dimension, indexed_count,
		       last_embedding_id, source_revision, model_config, vec1_version,
		       started_at, completed_at, last_error
		  FROM vector_accelerators WHERE generation_id = ?`, int64(generationID)).Scan(
		&a.GenerationID, &a.State, &a.TableName, &a.Dimension, &a.IndexedCount,
		&a.LastEmbeddingID, &a.SourceRevision, &a.ModelConfig, &a.Vec1Version,
		&startedAt, &completedAt, &lastError,
	)
	if errors.Is(err, sql.ErrNoRows) {
		//nolint:nilnil // A missing accelerator is represented by a nil status.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup accelerator: %w", err)
	}
	a.StartedAt = time.Unix(startedAt, 0)
	if completedAt.Valid {
		completed := time.Unix(completedAt.Int64, 0)
		a.CompletedAt = &completed
	}
	a.LastError = lastError.String
	return &a, nil
}

// EffectiveAccelerator returns the accelerator row for status surfaces with
// the effective display state. A stored-ready accelerator that fails any
// eligibility check search applies — kind, dimension or generation-dimension
// mismatch, indexed-count or vector-revision drift, table-name or Vec1
// version mismatch, or a missing physical table — is reported as stale
// instead of ready, so `embeddings list` never advertises an accelerator
// search would refuse. Building, already-stale, and never-optimized (nil)
// rows pass through unchanged, and an accelerator row left without its
// generation is reported stale rather than failing the whole listing. The
// check is read-only: stored state is never rewritten, and search keeps its
// own fail-open eligibility path.
func (b *Backend) EffectiveAccelerator(
	ctx context.Context,
	generationID vector.GenerationID,
) (*AcceleratorStatus, error) {
	status, err := b.Accelerator(ctx, generationID)
	if err != nil || status == nil || status.State != AcceleratorReady {
		return status, err
	}
	var dimension int
	err = b.db.QueryRowContext(ctx, `SELECT dimension FROM index_generations
		WHERE id = ?`, int64(generationID)).Scan(&dimension)
	if errors.Is(err, sql.ErrNoRows) {
		// An accelerator row whose generation is gone can never serve search.
		status.State = AcceleratorStale
		return status, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup generation %d for accelerator status: %w", generationID, err)
	}
	_, eligible, err := b.readyAccelerator(ctx, generationID, dimension)
	if err != nil {
		return nil, fmt.Errorf("check accelerator eligibility for generation %d: %w", generationID, err)
	}
	if !eligible {
		status.State = AcceleratorStale
	}
	return status, nil
}

// RecordAcceleratorError keeps a bounded diagnostic for status output while
// leaving the prepared copy restartable on the next optimize run.
func (b *Backend) RecordAcceleratorError(ctx context.Context, generationID vector.GenerationID, cause error) error {
	message := ""
	if cause != nil {
		message = strings.ToValidUTF8(cause.Error(), "�")
	}
	if len(message) > 1000 {
		message = strings.ToValidUTF8(message[:1000], "")
	}
	_, err := b.db.ExecContext(ctx, `UPDATE vector_accelerators SET last_error = ?
		WHERE generation_id = ? AND state = 'building'`, message, int64(generationID))
	if err != nil {
		return fmt.Errorf("record accelerator error: %w", err)
	}
	return nil
}

func acceleratorTableName(generationID vector.GenerationID) string {
	return fmt.Sprintf("message_ann_g%d", generationID)
}

func installedVec1Version(ctx context.Context, db *sql.DB) (string, error) {
	var info string
	if err := db.QueryRowContext(ctx, `SELECT vec1_info()`).Scan(&info); err != nil {
		return "", err
	}
	const prefix = "version "
	if !strings.HasPrefix(info, prefix) {
		return "", fmt.Errorf("unexpected vec1_info result %q", info)
	}
	version, _, _ := strings.Cut(strings.TrimPrefix(info, prefix), " ")
	return version, nil
}

// readyAccelerator performs only indexed metadata lookups. A missing, stale,
// mismatched, or legacy-invalidated accelerator is a normal exact-fallback
// condition, not an error.
func (b *Backend) readyAccelerator(
	ctx context.Context,
	generationID vector.GenerationID,
	dimension int,
) (AcceleratorStatus, bool, error) {
	a, err := b.readyAcceleratorQuery(ctx, b.db, generationID, dimension, "")
	if err != nil || a == nil {
		return AcceleratorStatus{}, false, err
	}
	return *a, true, nil
}

type rowQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (b *Backend) readyAcceleratorQuery(
	ctx context.Context,
	queryer rowQueryer,
	generationID vector.GenerationID,
	dimension int,
	schemaPrefix string,
) (*AcceleratorStatus, error) {
	var a AcceleratorStatus
	err := queryer.QueryRowContext(ctx, `
		SELECT a.generation_id, a.state, a.table_name, a.dimension,
		       a.indexed_count, a.last_embedding_id, a.source_revision,
		       a.model_config, a.vec1_version
		  FROM `+schemaPrefix+`vector_accelerators a
		  JOIN `+schemaPrefix+`index_generations g ON g.id = a.generation_id
		 WHERE a.generation_id = ?
		   AND a.kind = ?
		   AND a.state = 'ready'
		   AND a.dimension = ?
		   AND g.dimension = a.dimension
		   AND g.embedding_count = a.indexed_count
		   AND g.vector_revision = a.source_revision`,
		int64(generationID), acceleratorKind, dimension).Scan(
		&a.GenerationID, &a.State, &a.TableName, &a.Dimension,
		&a.IndexedCount, &a.LastEmbeddingID, &a.SourceRevision,
		&a.ModelConfig, &a.Vec1Version,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // Ineligible accelerators use exact vectors.
	}
	if err != nil {
		return nil, fmt.Errorf("lookup ready accelerator: %w", err)
	}
	if a.TableName != acceleratorTableName(generationID) ||
		a.Vec1Version != vec1.Version || a.Vec1Version != b.vec1Version {
		return nil, nil //nolint:nilnil // Ineligible accelerators use exact vectors.
	}
	exists, err := acceleratorTableExistsInSchema(ctx, queryer, a.TableName, schemaPrefix)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil //nolint:nilnil // Ineligible accelerators use exact vectors.
	}
	return &a, nil
}

func acceleratorTableExists(
	ctx context.Context,
	queryer rowQueryer,
	tableName string,
) (bool, error) {
	return acceleratorTableExistsInSchema(ctx, queryer, tableName, "")
}

func acceleratorTableExistsInSchema(ctx context.Context, queryer rowQueryer, tableName, schemaPrefix string) (bool, error) {
	var exists int
	err := queryer.QueryRowContext(ctx, `SELECT 1 FROM `+schemaPrefix+`sqlite_master
		WHERE type = 'table' AND name = ?`, tableName).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check accelerator table: %w", err)
	}
	return true, nil
}

// readyAcceleratorForWrite resolves eligibility inside the caller's write
// transaction. The returned table may be mutated only in that transaction;
// syncReadyAccelerator advances the captured source revision before commit.
func (b *Backend) readyAcceleratorForWrite(
	ctx context.Context,
	tx *sql.Tx,
	generationID vector.GenerationID,
	dimension int,
) (*AcceleratorStatus, error) {
	return b.readyAcceleratorQuery(ctx, tx, generationID, dimension, "")
}

func syncReadyAccelerator(ctx context.Context, tx *sql.Tx, accelerator *AcceleratorStatus) error {
	return syncReadyAcceleratorSchema(ctx, tx, accelerator, "")
}

func syncReadyAcceleratorSchema(
	ctx context.Context,
	tx *sql.Tx,
	accelerator *AcceleratorStatus,
	schemaPrefix string,
) error {
	if accelerator == nil {
		return nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE `+schemaPrefix+`vector_accelerators
		SET indexed_count = (
		        SELECT embedding_count FROM `+schemaPrefix+`index_generations WHERE id = ?
		    ),
		    source_revision = (
		        SELECT vector_revision FROM `+schemaPrefix+`index_generations WHERE id = ?
		    ),
		    last_embedding_id = COALESCE((
		        SELECT MAX(embedding_id) FROM `+schemaPrefix+`embeddings WHERE generation_id = ?
		    ), 0)
		WHERE generation_id = ? AND state = 'ready' AND table_name = ?
		  AND source_revision = ?`,
		int64(accelerator.GenerationID), int64(accelerator.GenerationID),
		int64(accelerator.GenerationID), int64(accelerator.GenerationID),
		accelerator.TableName, accelerator.SourceRevision)
	if err != nil {
		return fmt.Errorf("advance ready accelerator revision: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect ready accelerator revision update: %w", err)
	}
	if updated != 1 {
		return fmt.Errorf("%w: ready accelerator metadata changed during write", ErrAcceleratorSourceChanged)
	}
	return nil
}

// DropAccelerator removes the disposable Vec1 table and its build state.
// Authoritative vectors remain available for exact search and another build.
func (b *Backend) DropAccelerator(ctx context.Context, generationID vector.GenerationID) error {
	if generationID <= 0 {
		return fmt.Errorf("%w: %d", vector.ErrUnknownGeneration, generationID)
	}
	if _, err := b.generationVectorSnapshot(ctx, generationID); err != nil {
		return err
	}
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin accelerator drop: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+acceleratorTableName(generationID)); err != nil {
		return fmt.Errorf("drop accelerator table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM vector_accelerators WHERE generation_id = ?`, int64(generationID)); err != nil {
		return fmt.Errorf("delete accelerator metadata: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit accelerator drop: %w", err)
	}
	return nil
}
