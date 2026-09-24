//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec/vec1"
)

const (
	defaultOptimizeBatchSize = 5000
	minVec1TrainingRows      = 512
	optimizeDiskHeadroom     = 128 << 20
	maxVec1Threads           = 128
)

// ErrAcceleratorSourceChanged means authoritative vectors changed during a
// build. The partial table remains non-ready and the next prepare restarts it.
var ErrAcceleratorSourceChanged = errors.New("accelerator source changed")

// OptimizeProgress is emitted after one copied batch commits.
type OptimizeProgress struct {
	IndexedCount int64
	TotalCount   int64
}

// OptimizeOptions controls a SQLite accelerator build. Zero values select
// production defaults.
type OptimizeOptions struct {
	BatchSize int
	Threads   int
	Progress  func(OptimizeProgress)
}

// OptimizePlan describes the prepared raw Vec1 copy and its model parameters.
type OptimizePlan struct {
	Applicable     bool
	AlreadyReady   bool
	Restarted      bool
	Reason         string
	GenerationID   vector.GenerationID
	TableName      string
	IndexedCount   int64
	SourceRevision int64
	Threads        int
}

type acceleratorModelConfig struct {
	Distance     string `json:"distance"`
	Quantizer    string `json:"quantizer"`
	NBuckets     int    `json:"nbucket"`
	CodeSize     int    `json:"codesize"`
	SampleLimit  int64  `json:"sample_limit"`
	SampleStride int64  `json:"sample_stride"`
}

type generationVectorSnapshot struct {
	Dimension int
	State     vector.GenerationState
	Count     int64
	Revision  int64
}

type acceleratorVectorRow struct {
	id   int64
	blob []byte
}

func normalizeOptimizeOptions(options OptimizeOptions) OptimizeOptions {
	if options.BatchSize <= 0 {
		options.BatchSize = defaultOptimizeBatchSize
	}
	if options.Threads <= 0 {
		options.Threads = runtime.GOMAXPROCS(0)
	}
	options.Threads = normalizeVec1Threads(min(options.Threads, runtime.GOMAXPROCS(0)))
	return options
}

func normalizeVec1Threads(threads int) int {
	return min(max(threads, 1), maxVec1Threads)
}

func vec1DimensionOptimizable(dimension int) bool {
	return dimension >= vec1.MinDimension && dimension <= vec1.MaxDimension
}

func modelConfigFor(snapshot generationVectorSnapshot) acceleratorModelConfig {
	nBuckets := max(2, int(math.Round(math.Sqrt(float64(snapshot.Count)))))
	nBuckets = min(nBuckets, 65536)
	codeSize := min(32, snapshot.Dimension)
	sampleLimit := min(snapshot.Count, int64(100*nBuckets))
	stride := max(snapshot.Count/max(sampleLimit, 1), 1)
	return acceleratorModelConfig{
		Distance: "l2", Quantizer: "opq", NBuckets: nBuckets,
		CodeSize: codeSize, SampleLimit: sampleLimit, SampleStride: stride,
	}
}

func (b *Backend) generationVectorSnapshot(
	ctx context.Context,
	generationID vector.GenerationID,
) (generationVectorSnapshot, error) {
	var snapshot generationVectorSnapshot
	err := b.db.QueryRowContext(ctx, `SELECT dimension, state, embedding_count, vector_revision
		FROM index_generations WHERE id = ?`, int64(generationID)).Scan(
		&snapshot.Dimension, &snapshot.State, &snapshot.Count, &snapshot.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return generationVectorSnapshot{}, fmt.Errorf("%w: %d", vector.ErrUnknownGeneration, generationID)
	}
	if err != nil {
		return generationVectorSnapshot{}, fmt.Errorf("lookup generation %d: %w", generationID, err)
	}
	return snapshot, nil
}

// PrepareAccelerator creates or resumes the raw Vec1 copy. It reads existing
// float vectors only and never invokes an embedding provider.
func (b *Backend) PrepareAccelerator(
	ctx context.Context,
	generationID vector.GenerationID,
	options OptimizeOptions,
) (OptimizePlan, error) {
	if generationID <= 0 {
		return OptimizePlan{}, fmt.Errorf("%w: %d", vector.ErrUnknownGeneration, generationID)
	}
	options = normalizeOptimizeOptions(options)
	snapshot, err := b.generationVectorSnapshot(ctx, generationID)
	if err != nil {
		return OptimizePlan{}, err
	}
	if snapshot.State == vector.GenerationRetired {
		return OptimizePlan{}, fmt.Errorf("%w: %d", vector.ErrGenerationRetired, generationID)
	}
	if !vec1DimensionOptimizable(snapshot.Dimension) {
		return OptimizePlan{
			GenerationID: generationID,
			Reason: fmt.Sprintf("generation dimension %d outside Vec1 supported range %d-%d",
				snapshot.Dimension, vec1.MinDimension, vec1.MaxDimension),
		}, nil
	}
	config := modelConfigFor(snapshot)
	minimum := max(int64(minVec1TrainingRows), int64(4*config.NBuckets))
	if snapshot.Count < minimum {
		return OptimizePlan{
			GenerationID: generationID,
			Reason:       fmt.Sprintf("generation has %d vectors; Vec1 training requires at least %d", snapshot.Count, minimum),
		}, nil
	}
	if err := b.checkOptimizeDiskSpace(snapshot); err != nil {
		return OptimizePlan{}, err
	}

	configJSON, err := json.Marshal(config)
	if err != nil {
		return OptimizePlan{}, fmt.Errorf("encode accelerator model config: %w", err)
	}
	tableName := acceleratorTableName(generationID)
	plan := OptimizePlan{
		Applicable: true, GenerationID: generationID, TableName: tableName,
		SourceRevision: snapshot.Revision, Threads: options.Threads,
	}

	current, err := b.Accelerator(ctx, generationID)
	if err != nil {
		return OptimizePlan{}, err
	}
	if current != nil && current.State == AcceleratorReady &&
		current.TableName == tableName && current.Dimension == snapshot.Dimension &&
		current.IndexedCount == snapshot.Count && current.SourceRevision == snapshot.Revision &&
		current.ModelConfig == string(configJSON) && current.Vec1Version == b.vec1Version {
		_, ready, readyErr := b.readyAccelerator(ctx, generationID, snapshot.Dimension)
		if readyErr != nil {
			return OptimizePlan{}, readyErr
		}
		if ready {
			modelPresent, modelErr := b.acceleratorModelPresent(ctx, tableName)
			if modelErr != nil {
				return OptimizePlan{}, modelErr
			}
			if modelPresent {
				rowIDs, queryErr := b.acceleratedRowIDs(
					ctx, b.db, tableName, make([]float32, snapshot.Dimension), 1, 1,
				)
				if queryErr == nil && len(rowIDs) == 1 {
					plan.AlreadyReady = true
					plan.IndexedCount = current.IndexedCount
					return plan, nil
				}
			}
			if ctx.Err() != nil {
				return OptimizePlan{}, ctx.Err()
			}
		}
	}
	resume := current != nil && current.State == AcceleratorBuilding &&
		current.TableName == tableName && current.Dimension == snapshot.Dimension &&
		current.SourceRevision == snapshot.Revision && current.ModelConfig == string(configJSON) &&
		current.Vec1Version == b.vec1Version
	if resume {
		exists, existsErr := acceleratorTableExists(ctx, b.db, tableName)
		if existsErr != nil {
			return OptimizePlan{}, existsErr
		}
		resume = exists
	}
	if !resume {
		plan.Restarted = current != nil
		if err := b.resetAccelerator(ctx, generationID, tableName, snapshot, string(configJSON)); err != nil {
			return OptimizePlan{}, err
		}
		current, err = b.Accelerator(ctx, generationID)
		if err != nil {
			return OptimizePlan{}, err
		}
	}

	indexed := current.IndexedCount
	lastID := current.LastEmbeddingID
	for {
		if err := ctx.Err(); err != nil {
			return OptimizePlan{}, err
		}
		batch, err := b.readAcceleratorSourceBatch(
			ctx, generationID, snapshot.Dimension, lastID, options.BatchSize,
		)
		if err != nil {
			return OptimizePlan{}, err
		}
		if len(batch) == 0 {
			break
		}
		if err := b.insertAcceleratorBatch(ctx, current, batch); err != nil {
			return OptimizePlan{}, err
		}
		indexed += int64(len(batch))
		lastID = batch[len(batch)-1].id
		current.IndexedCount = indexed
		current.LastEmbeddingID = lastID
		if options.Progress != nil {
			options.Progress(OptimizeProgress{IndexedCount: indexed, TotalCount: snapshot.Count})
		}
	}

	after, err := b.generationVectorSnapshot(ctx, generationID)
	if err != nil {
		return OptimizePlan{}, err
	}
	if after.Count != snapshot.Count || after.Revision != snapshot.Revision {
		return OptimizePlan{}, fmt.Errorf("%w: generation %d changed from count/revision %d/%d to %d/%d",
			ErrAcceleratorSourceChanged, generationID,
			snapshot.Count, snapshot.Revision, after.Count, after.Revision)
	}
	var copied int64
	if err := b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+tableName).Scan(&copied); err != nil {
		return OptimizePlan{}, fmt.Errorf("count copied accelerator rows: %w", err)
	}
	if copied != snapshot.Count || indexed != snapshot.Count {
		return OptimizePlan{}, fmt.Errorf("accelerator copy count mismatch: source=%d metadata=%d table=%d",
			snapshot.Count, indexed, copied)
	}
	plan.IndexedCount = indexed
	return plan, nil
}

func (b *Backend) readAcceleratorSourceBatch(
	ctx context.Context,
	generationID vector.GenerationID,
	dimension int,
	lastID int64,
	batchSize int,
) (batch []acceleratorVectorRow, err error) {
	rows, err := b.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT v.embedding_id, v.embedding
		  FROM %s v
		  JOIN embeddings e ON e.embedding_id = v.embedding_id
		 WHERE v.generation_id = ? AND v.embedding_id > ?
		 ORDER BY v.embedding_id
		 LIMIT ?`, VectorTableName(dimension)),
		int64(generationID), lastID, batchSize)
	if err != nil {
		return nil, fmt.Errorf("read accelerator source batch: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close accelerator source rows: %w", closeErr)
		}
	}()

	batch = make([]acceleratorVectorRow, 0, batchSize)
	for rows.Next() {
		var row acceleratorVectorRow
		if scanErr := rows.Scan(&row.id, &row.blob); scanErr != nil {
			return nil, fmt.Errorf("scan accelerator source row: %w", scanErr)
		}
		batch = append(batch, row)
	}
	if rowErr := rows.Err(); rowErr != nil {
		return nil, fmt.Errorf("iterate accelerator source rows: %w", rowErr)
	}
	return batch, nil
}

func (b *Backend) acceleratorModelPresent(ctx context.Context, tableName string) (bool, error) {
	exists, err := acceleratorTableExists(ctx, b.db, tableName+"_model")
	if err != nil || !exists {
		return false, err
	}
	var present int
	err = b.db.QueryRowContext(ctx, `SELECT 1 FROM `+tableName+`_model WHERE id = 1`).Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, fmt.Errorf("inspect accelerator model: %w", err)
	}
	return true, nil
}

func (b *Backend) checkOptimizeDiskSpace(snapshot generationVectorSnapshot) error {
	if b.path == "" || b.path == ":memory:" || strings.HasPrefix(b.path, "file::memory:") {
		return nil
	}
	usage, err := disk.Usage(filepath.Dir(b.path))
	if err != nil {
		return fmt.Errorf("inspect accelerator disk space: %w", err)
	}
	if snapshot.Count < 0 || snapshot.Dimension < 0 {
		return errors.New("invalid negative accelerator size")
	}
	// #nosec G115 -- both values were explicitly checked non-negative above.
	count := uint64(snapshot.Count)
	// #nosec G115 -- both values were explicitly checked non-negative above.
	dimension := uint64(snapshot.Dimension)
	maxUint64 := ^uint64(0)
	if dimension > maxUint64/4 || (dimension > 0 && count > maxUint64/(dimension*4)) {
		return errors.New("accelerator size exceeds addressable disk estimate")
	}
	rawBytes := count * dimension * 4
	if rawBytes > (maxUint64-optimizeDiskHeadroom)/2 {
		return errors.New("accelerator disk estimate overflows")
	}
	required := rawBytes*2 + optimizeDiskHeadroom
	if usage.Free < required {
		return fmt.Errorf("insufficient disk space for accelerator: need at least %d bytes free, have %d", required, usage.Free)
	}
	return nil
}

func (b *Backend) resetAccelerator(
	ctx context.Context,
	generationID vector.GenerationID,
	tableName string,
	snapshot generationVectorSnapshot,
	configJSON string,
) error {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin accelerator reset: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+tableName); err != nil {
		return fmt.Errorf("drop partial accelerator: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE VIRTUAL TABLE `+tableName+` USING vec1(embedding)`); err != nil {
		return fmt.Errorf("create accelerator table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO vector_accelerators
		(generation_id, kind, state, table_name, dimension, indexed_count,
		 last_embedding_id, source_revision, started_at, completed_at,
		 model_config, vec1_version, last_error)
		VALUES (?, ?, 'building', ?, ?, 0, 0, ?, ?, NULL, ?, ?, NULL)
		ON CONFLICT(generation_id) DO UPDATE SET
			kind=excluded.kind, state=excluded.state, table_name=excluded.table_name,
			dimension=excluded.dimension, indexed_count=0, last_embedding_id=0,
			source_revision=excluded.source_revision, started_at=excluded.started_at,
			completed_at=NULL, model_config=excluded.model_config,
			vec1_version=excluded.vec1_version, last_error=NULL`,
		int64(generationID), acceleratorKind, tableName, snapshot.Dimension,
		snapshot.Revision, time.Now().Unix(), configJSON, b.vec1Version); err != nil {
		return fmt.Errorf("record accelerator build: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit accelerator reset: %w", err)
	}
	return nil
}

func (b *Backend) insertAcceleratorBatch(
	ctx context.Context,
	current *AcceleratorStatus,
	batch []acceleratorVectorRow,
) error {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin accelerator copy batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO `+current.TableName+`(rowid, embedding) VALUES (?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare accelerator insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	for _, row := range batch {
		if _, err := stmt.ExecContext(ctx, row.id, row.blob); err != nil {
			return fmt.Errorf("insert accelerator row %d: %w", row.id, err)
		}
	}
	lastID := batch[len(batch)-1].id
	result, err := tx.ExecContext(ctx, `UPDATE vector_accelerators
		SET indexed_count = indexed_count + ?, last_embedding_id = ?
		WHERE generation_id = ? AND state = 'building' AND source_revision = ?`,
		len(batch), lastID, int64(current.GenerationID), current.SourceRevision)
	if err != nil {
		return fmt.Errorf("advance accelerator copy: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect accelerator copy progress: %w", err)
	}
	if updated != 1 {
		return fmt.Errorf("%w: accelerator metadata changed during copy", ErrAcceleratorSourceChanged)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit accelerator copy batch: %w", err)
	}
	return nil
}

// RunAcceleratorWorker performs native Vec1 training and rebuild. Callers must
// isolate this in a disposable process; Vec1 0.7 cannot safely honor an
// in-process context interruption during OPQ training.
func RunAcceleratorWorker(
	ctx context.Context,
	databasePath string,
	generationID vector.GenerationID,
	threads int,
) error {
	if err := RegisterExtension(); err != nil {
		return err
	}
	db, err := sql.Open(DriverName(), databasePath)
	if err != nil {
		return fmt.Errorf("open accelerator worker database: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()

	var tableName, configJSON, storedVersion string
	var sourceRevision, currentRevision int64
	err = db.QueryRowContext(ctx, `SELECT a.table_name, a.model_config,
		a.vec1_version, a.source_revision, g.vector_revision
		FROM vector_accelerators a JOIN index_generations g ON g.id = a.generation_id
		WHERE a.generation_id = ? AND a.state = 'building'`, int64(generationID)).Scan(
		&tableName, &configJSON, &storedVersion, &sourceRevision, &currentRevision)
	if err != nil {
		return fmt.Errorf("load accelerator worker state: %w", err)
	}
	if tableName != acceleratorTableName(generationID) || storedVersion != vec1.Version ||
		sourceRevision != currentRevision {
		return fmt.Errorf("%w: accelerator worker metadata mismatch", ErrAcceleratorSourceChanged)
	}
	var config acceleratorModelConfig
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		return fmt.Errorf("decode accelerator model config: %w", err)
	}
	threads = normalizeVec1Threads(threads)
	var configured any
	if err := db.QueryRowContext(ctx, `SELECT vec1_config('nthread', ?)`, threads).Scan(&configured); err != nil {
		return fmt.Errorf("configure Vec1 worker threads: %w", err)
	}
	trainingConfig, err := json.Marshal(map[string]any{
		"distance": config.Distance, "quantizer": config.Quantizer,
		"nbucket": config.NBuckets, "codesize": config.CodeSize,
	})
	if err != nil {
		return fmt.Errorf("encode Vec1 training config: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin Vec1 training: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var model []byte
	query := fmt.Sprintf(`SELECT vec1_train(embedding, ?) FROM (
		SELECT embedding FROM (
			SELECT embedding, ROW_NUMBER() OVER (ORDER BY rowid) AS sample_ordinal
			  FROM %s
		) WHERE ((sample_ordinal - 1) %% ?) = 0
		  ORDER BY sample_ordinal LIMIT ?
	)`, tableName)
	if err := tx.QueryRowContext(ctx, query, string(trainingConfig), config.SampleStride, config.SampleLimit).Scan(&model); err != nil {
		return fmt.Errorf("train Vec1 model: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+tableName+`(cmd, arg) VALUES ('rebuild', ?)`, model); err != nil {
		return fmt.Errorf("rebuild Vec1 accelerator: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit Vec1 accelerator rebuild: %w", err)
	}
	return nil
}

// PublishAccelerator runs explicit full diagnostics and atomically makes a
// successfully trained accelerator eligible for search.
func (b *Backend) PublishAccelerator(
	ctx context.Context,
	generationID vector.GenerationID,
) (AcceleratorStatus, error) {
	status, err := b.Accelerator(ctx, generationID)
	if err != nil {
		return AcceleratorStatus{}, err
	}
	if status == nil || status.State != AcceleratorBuilding ||
		status.TableName != acceleratorTableName(generationID) {
		return AcceleratorStatus{}, fmt.Errorf("generation %d has no prepared accelerator", generationID)
	}
	snapshot, err := b.generationVectorSnapshot(ctx, generationID)
	if err != nil {
		return AcceleratorStatus{}, err
	}
	if snapshot.Revision != status.SourceRevision || snapshot.Count != status.IndexedCount {
		return AcceleratorStatus{}, fmt.Errorf("%w: generation %d changed before publication", ErrAcceleratorSourceChanged, generationID)
	}
	if err := b.fullIntegrityCheck(ctx); err != nil {
		return AcceleratorStatus{}, err
	}
	var count, minID, maxID int64
	if err := b.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MIN(rowid), 0), COALESCE(MAX(rowid), 0)
		FROM `+status.TableName).Scan(&count, &minID, &maxID); err != nil {
		return AcceleratorStatus{}, fmt.Errorf("verify accelerator rows: %w", err)
	}
	if count != snapshot.Count || maxID != status.LastEmbeddingID {
		return AcceleratorStatus{}, fmt.Errorf("accelerator verification mismatch: source=%d table=%d max_rowid=%d metadata_max=%d",
			snapshot.Count, count, maxID, status.LastEmbeddingID)
	}
	if count > 0 {
		var sourceMin int64
		if err := b.db.QueryRowContext(ctx, `SELECT MIN(embedding_id) FROM embeddings WHERE generation_id = ?`,
			int64(generationID)).Scan(&sourceMin); err != nil {
			return AcceleratorStatus{}, fmt.Errorf("verify source row bounds: %w", err)
		}
		if minID != sourceMin {
			return AcceleratorStatus{}, fmt.Errorf("accelerator minimum rowid mismatch: source=%d table=%d", sourceMin, minID)
		}
	}
	result, err := b.db.ExecContext(ctx, `UPDATE vector_accelerators
		SET state = 'ready', completed_at = ?, last_error = NULL
		WHERE generation_id = ? AND state = 'building' AND source_revision = ?`,
		time.Now().Unix(), int64(generationID), snapshot.Revision)
	if err != nil {
		return AcceleratorStatus{}, fmt.Errorf("publish accelerator: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return AcceleratorStatus{}, fmt.Errorf("inspect accelerator publication: %w", err)
	}
	if updated != 1 {
		return AcceleratorStatus{}, fmt.Errorf("%w: accelerator publication lost its source revision", ErrAcceleratorSourceChanged)
	}
	published, err := b.Accelerator(ctx, generationID)
	if err != nil {
		return AcceleratorStatus{}, err
	}
	return *published, nil
}

func (b *Backend) fullIntegrityCheck(ctx context.Context) error {
	rows, err := b.db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return fmt.Errorf("run vector integrity check: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return fmt.Errorf("scan vector integrity result: %w", err)
		}
		if result != "ok" {
			return fmt.Errorf("vector integrity check failed: %s", result)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate vector integrity results: %w", err)
	}
	return nil
}
