//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec/vec1"
)

func TestNormalizeOptimizeOptionsCapsVec1Threads(t *testing.T) {
	previous := runtime.GOMAXPROCS(256)
	t.Cleanup(func() { runtime.GOMAXPROCS(previous) })

	options := normalizeOptimizeOptions(OptimizeOptions{Threads: 256})
	assert.Equal(t, 128, options.Threads)
	assert.Equal(t, 128, normalizeVec1Threads(256))
}

func TestVec1DimensionOptimizableBounds(t *testing.T) {
	assert.False(t, vec1DimensionOptimizable(1))
	assert.True(t, vec1DimensionOptimizable(2))
	assert.True(t, vec1DimensionOptimizable(vec1.MaxDimension))
	assert.False(t, vec1DimensionOptimizable(vec1.MaxDimension+1))
}

func TestPrepareAcceleratorRejectsDimensionOutsideVec1Range(t *testing.T) {
	for _, dimension := range []int{1, vec1.MaxDimension + 1} {
		t.Run(strconv.Itoa(dimension), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			backend := openOptimizeBackend(t, dimension)
			generationID := seedOptimizeVectors(t, backend, 512, dimension)

			plan, err := backend.PrepareAccelerator(t.Context(), generationID, OptimizeOptions{})
			require.NoError(err)
			assert.False(plan.Applicable)
			assert.Empty(plan.TableName)
			assert.Contains(plan.Reason, "dimension")

			var accelerators int
			require.NoError(backend.db.QueryRow(
				`SELECT COUNT(*) FROM vector_accelerators`).Scan(&accelerators))
			assert.Zero(accelerators)
			exists, err := acceleratorTableExists(
				t.Context(), backend.db, acceleratorTableName(generationID))
			require.NoError(err)
			assert.False(exists)

			retry, err := backend.PrepareAccelerator(t.Context(), generationID, OptimizeOptions{})
			require.NoError(err)
			assert.Equal(plan, retry)
		})
	}
}

func TestPrepareAcceleratorCopiesExistingVectors(t *testing.T) {
	backend := openOptimizeBackend(t, 32)
	generationID := seedOptimizeVectors(t, backend, 512, 32)

	plan, err := backend.PrepareAccelerator(t.Context(), generationID, OptimizeOptions{BatchSize: 73})
	require.NoError(t, err)
	require.True(t, plan.Applicable)
	assert.False(t, plan.AlreadyReady)
	assert.Equal(t, int64(512), plan.IndexedCount)
	assert.Equal(t, acceleratorTableName(generationID), plan.TableName)

	var count int64
	require.NoError(t, backend.db.QueryRow(`SELECT COUNT(*) FROM `+plan.TableName).Scan(&count))
	assert.Equal(t, int64(512), count)

	var authoritative, copied []byte
	require.NoError(t, backend.db.QueryRow(`SELECT embedding FROM vectors_vec_d32
		WHERE generation_id = ? AND embedding_id = 257`, int64(generationID)).Scan(&authoritative))
	require.NoError(t, backend.db.QueryRow(`SELECT embedding FROM `+plan.TableName+
		` WHERE rowid = 257`).Scan(&copied))
	assert.Equal(t, authoritative, copied)
}

func TestPrepareAcceleratorLeavesSmallGenerationExact(t *testing.T) {
	backend := openOptimizeBackend(t, 4)
	generationID := seedOptimizeVectors(t, backend, 1, 4)

	plan, err := backend.PrepareAccelerator(t.Context(), generationID, OptimizeOptions{})
	require.NoError(t, err)
	assert.False(t, plan.Applicable)
	assert.Contains(t, plan.Reason, "512")

	var count int
	require.NoError(t, backend.db.QueryRow(`SELECT COUNT(*) FROM vector_accelerators`).Scan(&count))
	assert.Zero(t, count)
}

func TestPrepareAcceleratorLeavesEmptyGenerationExact(t *testing.T) {
	backend := openOptimizeBackend(t, 32)
	generationID, err := backend.CreateGeneration(t.Context(), "model", 32, "model:empty")
	require.NoError(t, err)

	plan, err := backend.PrepareAccelerator(t.Context(), generationID, OptimizeOptions{})
	require.NoError(t, err)
	assert.False(t, plan.Applicable)
	assert.Contains(t, plan.Reason, "512")

	var count int
	require.NoError(t, backend.db.QueryRow(`SELECT COUNT(*) FROM vector_accelerators`).Scan(&count))
	assert.Zero(t, count)
}

func TestPrepareAcceleratorRestartsWhenSourceChangedBetweenRuns(t *testing.T) {
	backend := openOptimizeBackend(t, 32)
	generationID := seedOptimizeVectors(t, backend, 512, 32)

	ctx, cancel := context.WithCancel(context.Background())
	_, err := backend.PrepareAccelerator(ctx, generationID, OptimizeOptions{
		BatchSize: 100,
		Progress: func(progress OptimizeProgress) {
			if progress.IndexedCount >= 100 {
				cancel()
			}
		},
	})
	require.ErrorIs(t, err, context.Canceled)

	// Replace an already-copied low rowid. The next run must discard the
	// watermark, not blindly resume above it.
	require.NoError(t, backend.Upsert(t.Context(), generationID, []vector.Chunk{
		{MessageID: 1, Vector: unitVec(32, 31)},
	}))

	plan, err := backend.PrepareAccelerator(t.Context(), generationID, OptimizeOptions{BatchSize: 91})
	require.NoError(t, err)
	require.True(t, plan.Applicable)
	assert.True(t, plan.Restarted)
	assert.Equal(t, int64(512), plan.IndexedCount)

	var authoritative, copied []byte
	require.NoError(t, backend.db.QueryRow(`SELECT v.embedding
		FROM vectors_vec_d32 v JOIN embeddings e ON e.embedding_id = v.embedding_id
		WHERE v.generation_id = ? AND e.message_id = 1`, int64(generationID)).Scan(&authoritative))
	var embeddingID int64
	require.NoError(t, backend.db.QueryRow(`SELECT embedding_id FROM embeddings
		WHERE generation_id = ? AND message_id = 1`, int64(generationID)).Scan(&embeddingID))
	require.NoError(t, backend.db.QueryRow(`SELECT embedding FROM `+plan.TableName+
		` WHERE rowid = ?`, embeddingID).Scan(&copied))
	assert.Equal(t, authoritative, copied)
}

func TestTrainAndPublishAccelerator(t *testing.T) {
	backend := openOptimizeBackend(t, 32)
	generationID := seedOptimizeVectors(t, backend, 512, 32)
	plan, err := backend.PrepareAccelerator(t.Context(), generationID, OptimizeOptions{Threads: 1})
	require.NoError(t, err)
	require.True(t, plan.Applicable)

	require.NoError(t, RunAcceleratorWorker(t.Context(), backend.path, generationID, 1))
	status, err := backend.PublishAccelerator(t.Context(), generationID)
	require.NoError(t, err)
	assert.Equal(t, AcceleratorReady, status.State)

	ready, ok, err := backend.readyAccelerator(t.Context(), generationID, 32)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, plan.TableName, ready.TableName)
}

func TestAcceleratorWorkerSamplesSparseRowIDs(t *testing.T) {
	backend := openOptimizeBackend(t, 32)
	generationID, err := backend.CreateGeneration(t.Context(), "model", 32, "model:sparse")
	require.NoError(t, err)
	tableName := acceleratorTableName(generationID)
	config := acceleratorModelConfig{
		Distance: "l2", Quantizer: "opq", NBuckets: 2, CodeSize: 32,
		SampleLimit: 512, SampleStride: 3,
	}
	configJSON, err := json.Marshal(config)
	require.NoError(t, err)
	_, err = backend.db.Exec(`CREATE VIRTUAL TABLE ` + tableName + ` USING vec1(embedding)`)
	require.NoError(t, err)
	_, err = backend.db.Exec(`INSERT INTO vector_accelerators
		(generation_id, kind, state, table_name, dimension, indexed_count,
		 last_embedding_id, source_revision, started_at, model_config, vec1_version)
		VALUES (?, ?, 'building', ?, 32, 1536, 4606, 0, 1, ?, ?)`,
		int64(generationID), acceleratorKind, tableName, string(configJSON), backend.vec1Version)
	require.NoError(t, err)
	tx, err := backend.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	stmt, err := tx.Prepare(`INSERT INTO ` + tableName + `(rowid, embedding) VALUES (?, ?) `)
	require.NoError(t, err)
	defer func() { _ = stmt.Close() }()
	for i := range 1536 {
		_, err = stmt.Exec(3*i+1, float32SliceBlob(unitVec(32, i%32)))
		require.NoError(t, err)
	}
	require.NoError(t, stmt.Close())
	require.NoError(t, tx.Commit())

	require.NoError(t, RunAcceleratorWorker(t.Context(), backend.path, generationID, 1))
}

func TestPrepareAcceleratorRepairsMissingReadyTable(t *testing.T) {
	backend := openOptimizeBackend(t, 32)
	generationID := seedOptimizeVectors(t, backend, 512, 32)
	plan, err := backend.PrepareAccelerator(t.Context(), generationID, OptimizeOptions{Threads: 1})
	require.NoError(t, err)
	require.NoError(t, RunAcceleratorWorker(t.Context(), backend.path, generationID, 1))
	_, err = backend.PublishAccelerator(t.Context(), generationID)
	require.NoError(t, err)
	_, err = backend.db.Exec(`DROP TABLE ` + plan.TableName)
	require.NoError(t, err)

	plan, err = backend.PrepareAccelerator(t.Context(), generationID, OptimizeOptions{Threads: 1})
	require.NoError(t, err)
	assert.False(t, plan.AlreadyReady)
	assert.True(t, plan.Restarted)
	assert.Equal(t, int64(512), plan.IndexedCount)
	status, err := backend.Accelerator(t.Context(), generationID)
	require.NoError(t, err)
	require.NotNil(t, status)
	assert.Equal(t, AcceleratorBuilding, status.State)
}

func TestPrepareAcceleratorRepairsReadyTableWithMissingModel(t *testing.T) {
	backend := openOptimizeBackend(t, 32)
	generationID := seedOptimizeVectors(t, backend, 512, 32)
	plan, err := backend.PrepareAccelerator(t.Context(), generationID, OptimizeOptions{Threads: 1})
	require.NoError(t, err)
	require.NoError(t, RunAcceleratorWorker(t.Context(), backend.path, generationID, 1))
	_, err = backend.PublishAccelerator(t.Context(), generationID)
	require.NoError(t, err)
	_, err = backend.db.Exec(`DROP TABLE ` + plan.TableName + `_model`)
	require.NoError(t, err)

	plan, err = backend.PrepareAccelerator(t.Context(), generationID, OptimizeOptions{Threads: 1})
	require.NoError(t, err)
	assert.False(t, plan.AlreadyReady)
	assert.True(t, plan.Restarted)
	assert.Equal(t, int64(512), plan.IndexedCount)
}

func TestPrepareAcceleratorRestartsWhenBuildingTableMissing(t *testing.T) {
	backend := openOptimizeBackend(t, 32)
	generationID := seedOptimizeVectors(t, backend, 512, 32)
	ctx, cancel := context.WithCancel(context.Background())
	_, err := backend.PrepareAccelerator(ctx, generationID, OptimizeOptions{
		BatchSize: 100,
		Progress: func(progress OptimizeProgress) {
			if progress.IndexedCount >= 100 {
				cancel()
			}
		},
	})
	require.ErrorIs(t, err, context.Canceled)
	_, err = backend.db.Exec(`DROP TABLE ` + acceleratorTableName(generationID))
	require.NoError(t, err)

	plan, err := backend.PrepareAccelerator(t.Context(), generationID, OptimizeOptions{BatchSize: 100})
	require.NoError(t, err)
	assert.True(t, plan.Restarted)
	assert.Equal(t, int64(512), plan.IndexedCount)
}

func TestPrepareAcceleratorDetectsConcurrentSourceChange(t *testing.T) {
	backend := openOptimizeBackend(t, 32)
	generationID := seedOptimizeVectors(t, backend, 512, 32)

	changed := false
	_, err := backend.PrepareAccelerator(t.Context(), generationID, OptimizeOptions{
		BatchSize: 100,
		Progress: func(progress OptimizeProgress) {
			if changed {
				return
			}
			changed = true
			require.NoError(t, backend.Upsert(context.Background(), generationID, []vector.Chunk{
				{MessageID: 1, Vector: unitVec(32, 1)},
			}))
		},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAcceleratorSourceChanged)
}

func TestAcceleratorWorkerCanBeKilledAndRetried(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vectors.db")
	backend, err := Open(t.Context(), Options{Path: path, Dimension: 64})
	require.NoError(t, err)
	generationID := seedOptimizeVectors(t, backend, 5000, 64)
	_, err = backend.PrepareAccelerator(t.Context(), generationID, OptimizeOptions{Threads: 1})
	require.NoError(t, err)
	require.NoError(t, backend.Close())

	marker := filepath.Join(t.TempDir(), "worker-started")
	ctx, cancel := context.WithCancel(context.Background())
	// #nosec G702 -- os.Args[0] is the current test binary, not external input.
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAcceleratorWorkerProcessHelper$")
	command.Env = append(os.Environ(),
		"MSGVAULT_TEST_ACCELERATOR_WORKER=1",
		"MSGVAULT_TEST_ACCELERATOR_PATH="+path,
		"MSGVAULT_TEST_ACCELERATOR_GENERATION="+strconv.FormatInt(int64(generationID), 10),
		"MSGVAULT_TEST_ACCELERATOR_MARKER="+marker,
	)
	require.NoError(t, command.Start())
	require.Eventually(t, func() bool {
		_, statErr := os.Stat(marker)
		return statErr == nil
	}, 5*time.Second, 10*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	cancel()
	require.Error(t, command.Wait())
	require.NotNil(t, command.ProcessState)
	assert.False(t, command.ProcessState.Success())

	backend, err = Open(t.Context(), Options{Path: path, Dimension: 64})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, backend.Close()) })
	status, err := backend.Accelerator(t.Context(), generationID)
	require.NoError(t, err)
	require.NotNil(t, status)
	assert.Equal(t, AcceleratorBuilding, status.State)

	require.NoError(t, RunAcceleratorWorker(t.Context(), path, generationID, 1))
	statusValue, err := backend.PublishAccelerator(t.Context(), generationID)
	require.NoError(t, err)
	assert.Equal(t, AcceleratorReady, statusValue.State)
}

func TestAcceleratorWorkerProcessHelper(t *testing.T) {
	if os.Getenv("MSGVAULT_TEST_ACCELERATOR_WORKER") != "1" {
		return
	}
	generationID, err := strconv.ParseInt(os.Getenv("MSGVAULT_TEST_ACCELERATOR_GENERATION"), 10, 64)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(os.Getenv("MSGVAULT_TEST_ACCELERATOR_MARKER"), []byte("started"), 0o600))
	require.NoError(t, RunAcceleratorWorker(
		context.Background(),
		os.Getenv("MSGVAULT_TEST_ACCELERATOR_PATH"),
		vector.GenerationID(generationID),
		1,
	))
}

func openOptimizeBackend(t *testing.T, dimension int) *Backend {
	t.Helper()
	backend, err := Open(t.Context(), Options{
		Path:      filepath.Join(t.TempDir(), "vectors.db"),
		Dimension: dimension,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, backend.Close()) })
	return backend
}

func seedOptimizeVectors(t *testing.T, backend *Backend, count, dimension int) vector.GenerationID {
	t.Helper()
	generationID, err := backend.CreateGeneration(t.Context(), "model", dimension, "model:test")
	require.NoError(t, err)
	chunks := make([]vector.Chunk, 0, count)
	for i := range count {
		values := make([]float32, dimension)
		values[i%dimension] = 1
		values[(i*7+3)%dimension] += float32(i%17) / 100
		chunks = append(chunks, vector.Chunk{MessageID: int64(i + 1), Vector: values})
	}
	require.NoError(t, backend.Upsert(t.Context(), generationID, chunks))
	return generationID
}

func TestDropAcceleratorPreservesExactVectors(t *testing.T) {
	backend, gen, _ := openAcceleratedSearchBackend(t, 2, 4, Options{})
	require.NoError(t, backend.Upsert(t.Context(), gen, []vector.Chunk{
		{MessageID: 1, Vector: unitVec(4, 0)}, {MessageID: 2, Vector: unitVec(4, 1)},
	}))
	table := installReadyFlatAccelerator(t, backend, gen, 4)
	require.NoError(t, backend.DropAccelerator(t.Context(), gen))
	require.NoError(t, backend.DropAccelerator(t.Context(), gen), "dropping twice is harmless")
	exists, err := acceleratorTableExists(t.Context(), backend.db, table)
	require.NoError(t, err)
	assert.False(t, exists)
	status, err := backend.Accelerator(t.Context(), gen)
	require.NoError(t, err)
	assert.Nil(t, status)
	hits, meta, err := backend.SearchWithMetadata(t.Context(), gen, unitVec(4, 0), 2, vector.Filter{})
	require.NoError(t, err)
	assert.Len(t, hits, 2)
	assert.Equal(t, "exact", meta.Accelerator)
}
