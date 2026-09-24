//go:build sqlite_vec

package sqlitevec

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"math"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

func TestAcceleratedSearchReranksFiltersAndDeduplicates(t *testing.T) {
	backend, generationID, mainDB := openAcceleratedSearchBackend(t, 4, 4, Options{})
	require.NoError(t, backend.Upsert(t.Context(), generationID, []vector.Chunk{
		{MessageID: 1, ChunkIndex: 0, Vector: []float32{0, 1, 0, 0}},
		{MessageID: 1, ChunkIndex: 1, Vector: []float32{1, 0, 0, 0}},
		{MessageID: 2, Vector: []float32{0.8, 0.2, 0, 0}},
		{MessageID: 3, Vector: []float32{0, 1, 0, 0}},
		{MessageID: 4, Vector: []float32{0, 0, 1, 0}},
	}))
	installReadyFlatAccelerator(t, backend, generationID, 4)

	hits, meta, err := backend.SearchWithMetadata(t.Context(), generationID,
		[]float32{1, 0, 0, 0}, 2, vector.Filter{})
	require.NoError(t, err)
	require.Len(t, hits, 2)
	assert.Equal(t, []int64{1, 2}, []int64{hits[0].MessageID, hits[1].MessageID})
	assert.InDelta(t, 1-math.Sqrt(0.08), hits[1].Score, 0.0001, "scores retain the exact-search L2 scale")
	assert.Equal(t, acceleratorKind, meta.Accelerator)
	assert.True(t, meta.PoolSaturated)

	_, err = mainDB.Exec(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = 1`)
	require.NoError(t, err)
	hits, _, err = backend.SearchWithMetadata(t.Context(), generationID,
		[]float32{1, 0, 0, 0}, 2, vector.Filter{SourceIDs: []int64{2, 3}})
	require.NoError(t, err)
	require.Len(t, hits, 2)
	assert.Equal(t, []int64{2, 3}, []int64{hits[0].MessageID, hits[1].MessageID})
}

func TestExactSearchMetadataReportsFullPageSaturation(t *testing.T) {
	backend, generationID, _ := openAcceleratedSearchBackend(t, 2, 4, Options{})
	require.NoError(t, backend.Upsert(t.Context(), generationID, []vector.Chunk{
		{MessageID: 1, Vector: unitVec(4, 0)},
		{MessageID: 2, Vector: unitVec(4, 1)},
	}))

	for _, mode := range []string{"auto-without-index", "exact"} {
		t.Run(mode, func(t *testing.T) {
			backend.acceleratorMode = mode
			if mode == "auto-without-index" {
				backend.acceleratorMode = "auto"
			}
			hits, metadata, err := backend.SearchWithMetadata(
				t.Context(), generationID, unitVec(4, 0), 1, vector.Filter{},
			)
			require.NoError(t, err)
			require.Len(t, hits, 1)
			assert.True(t, metadata.PoolSaturated)
		})
	}

	hits, metadata, err := backend.SearchWithMetadata(
		t.Context(), generationID, unitVec(4, 0), 3, vector.Filter{},
	)
	require.NoError(t, err)
	require.Len(t, hits, 2)
	assert.False(t, metadata.PoolSaturated)
}

func TestAcceleratedSearchUsesExactPathForSmallExplicitPopulation(t *testing.T) {
	backend, generationID, _ := openAcceleratedSearchBackend(t, 3, 4, Options{})
	require.NoError(t, backend.Upsert(t.Context(), generationID, []vector.Chunk{
		{MessageID: 1, Vector: unitVec(4, 0)},
		{MessageID: 2, Vector: []float32{0.9, 0.1, 0, 0}},
		{MessageID: 3, Vector: unitVec(4, 1)},
	}))
	installReadyFlatAccelerator(t, backend, generationID, 4)

	hits, meta, err := backend.SearchWithMetadata(t.Context(), generationID,
		unitVec(4, 0), 2, vector.Filter{MessageIDs: []int64{2, 3}})
	require.NoError(t, err)
	require.Len(t, hits, 2)
	assert.Equal(t, []int64{2, 3}, []int64{hits[0].MessageID, hits[1].MessageID})
	assert.Equal(t, "exact-filter", meta.Accelerator)
}

func TestAcceleratedSearchUsesExactPathForSmallStructuredPopulation(t *testing.T) {
	backend, generationID, mainDB := openAcceleratedSearchBackend(t, 300, 4, Options{ANNWorkCeiling: 64})
	chunks := make([]vector.Chunk, 0, 300)
	for i := range 300 {
		value := unitVec(4, 0)
		if i < 5 {
			value = unitVec(4, 1)
		}
		chunks = append(chunks, vector.Chunk{MessageID: int64(i + 1), Vector: value})
	}
	require.NoError(t, backend.Upsert(t.Context(), generationID, chunks))
	installReadyFlatAccelerator(t, backend, generationID, 4)
	_, err := mainDB.Exec(`UPDATE messages SET source_id = 1000 WHERE id <= 5`)
	require.NoError(t, err)

	filter := vector.Filter{SourceIDs: []int64{1000}}
	exactHits, err := backend.searchExact(t.Context(), generationID, unitVec(4, 0), 5, filter)
	require.NoError(t, err)
	acceleratedHits, metadata, err := backend.SearchWithMetadata(
		t.Context(), generationID, unitVec(4, 0), 5, filter,
	)
	require.NoError(t, err)

	require.Len(t, acceleratedHits, len(exactHits))
	for i := range exactHits {
		assert.Equal(t, exactHits[i].MessageID, acceleratedHits[i].MessageID)
		assert.Equal(t, exactHits[i].Rank, acceleratedHits[i].Rank)
		assert.InDelta(t, exactHits[i].Score, acceleratedHits[i].Score, 0.0001)
	}
	assert.Equal(t, "exact-filter", metadata.Accelerator)
}

func TestAcceleratedSearchUsesExactPathForBoundedStructuredPopulation(t *testing.T) {
	backend, generationID, mainDB := openAcceleratedSearchBackend(t, 600, 4, Options{})
	chunks := make([]vector.Chunk, 0, 600)
	for i := range 600 {
		chunks = append(chunks, vector.Chunk{
			MessageID: int64(i + 1), Vector: []float32{1, float32(i) / 1000, 0, 0},
		})
	}
	require.NoError(t, backend.Upsert(t.Context(), generationID, chunks))
	installReadyFlatAccelerator(t, backend, generationID, 4)
	_, err := mainDB.Exec(`UPDATE messages SET source_id = 1000 WHERE id <= 300`)
	require.NoError(t, err)

	filter := vector.Filter{SourceIDs: []int64{1000}}
	exactHits, err := backend.searchExact(t.Context(), generationID, unitVec(4, 0), 10, filter)
	require.NoError(t, err)
	acceleratedHits, metadata, err := backend.SearchWithMetadata(
		t.Context(), generationID, unitVec(4, 0), 10, filter,
	)
	require.NoError(t, err)

	assert.Equal(t, exactHits, acceleratedHits)
	assert.Equal(t, "exact-filter", metadata.Accelerator)
}

func TestExactFilterPopulationEligibilityCapsVectorRows(t *testing.T) {
	backend, generationID, _ := openAcceleratedSearchBackend(t, 1, 4, Options{})
	require.NoError(t, backend.Upsert(t.Context(), generationID, []vector.Chunk{
		{MessageID: 1, ChunkIndex: 0, Vector: unitVec(4, 0)},
		{MessageID: 1, ChunkIndex: 1, Vector: unitVec(4, 1)},
		{MessageID: 1, ChunkIndex: 2, Vector: unitVec(4, 2)},
	}))

	fits, err := backend.exactFilterPopulationFits(
		t.Context(), backend.db, generationID, []int64{1}, 2,
	)
	require.NoError(t, err)
	assert.False(t, fits)
	fits, err = backend.exactFilterPopulationFits(
		t.Context(), backend.db, generationID, []int64{1}, 3,
	)
	require.NoError(t, err)
	assert.True(t, fits)
}

func TestExactFilterSearchBoundsAllocationByPopulation(t *testing.T) {
	backend, generationID, _ := openAcceleratedSearchBackend(t, 1, 4, Options{})
	require.NoError(t, backend.Upsert(t.Context(), generationID, []vector.Chunk{
		{MessageID: 1, Vector: unitVec(4, 0)},
	}))
	installReadyFlatAccelerator(t, backend, generationID, 4)

	hits, metadata, err := backend.SearchWithMetadata(
		t.Context(), generationID, unitVec(4, 0), int(^uint(0)>>1),
		vector.Filter{MessageIDs: []int64{1}},
	)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, int64(1), hits[0].MessageID)
	assert.Equal(t, "exact-filter", metadata.Accelerator)
}

func TestAcceleratedSearchWidensAndReportsWorkCeiling(t *testing.T) {
	backend, generationID, mainDB := openAcceleratedSearchBackend(t, 80, 4, Options{ANNWorkCeiling: 64})
	chunks := make([]vector.Chunk, 0, 80)
	for i := range 80 {
		chunks = append(chunks, vector.Chunk{
			MessageID: int64(i + 1), Vector: []float32{1, float32(i) / 100, 0, 0},
		})
	}
	require.NoError(t, backend.Upsert(t.Context(), generationID, chunks))
	installReadyFlatAccelerator(t, backend, generationID, 4)
	_, err := mainDB.Exec(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id <= 40`)
	require.NoError(t, err)

	hits, meta, err := backend.SearchWithMetadata(t.Context(), generationID,
		unitVec(4, 0), 5, vector.Filter{})
	require.NoError(t, err)
	require.Len(t, hits, 5)
	assert.Equal(t, int64(41), hits[0].MessageID)
	assert.True(t, meta.PoolSaturated)

	_, err = mainDB.Exec(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP`)
	require.NoError(t, err)
	hits, meta, err = backend.SearchWithMetadata(t.Context(), generationID,
		unitVec(4, 0), 5, vector.Filter{})
	require.NoError(t, err)
	assert.Empty(t, hits)
	assert.True(t, meta.PoolSaturated)
}

func TestSearchDelegatesToReadyAccelerator(t *testing.T) {
	backend, generationID, mainDB := openAcceleratedSearchBackend(t, 80, 4, Options{ANNWorkCeiling: 32})
	chunks := make([]vector.Chunk, 0, 80)
	for i := range 80 {
		chunks = append(chunks, vector.Chunk{
			MessageID: int64(i + 1), Vector: []float32{1, float32(i) / 100, 0, 0},
		})
	}
	require.NoError(t, backend.Upsert(t.Context(), generationID, chunks))
	installReadyFlatAccelerator(t, backend, generationID, 4)
	_, err := mainDB.Exec(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id <= 40`)
	require.NoError(t, err)

	hits, err := backend.Search(t.Context(), generationID, unitVec(4, 0), 5, vector.Filter{})
	require.NoError(t, err)
	assert.Empty(t, hits, "the public search path must retain the accelerator work ceiling")
}

func TestAcceleratedSearchDoesNotFallbackAfterCancellation(t *testing.T) {
	backend, generationID, _ := openAcceleratedSearchBackend(t, 1, 4, Options{})
	require.NoError(t, backend.Upsert(t.Context(), generationID, []vector.Chunk{{
		MessageID: 1, Vector: unitVec(4, 0),
	}}))
	installReadyFlatAccelerator(t, backend, generationID, 4)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, _, err := backend.SearchWithMetadata(ctx, generationID, unitVec(4, 0), 1, vector.Filter{})
	require.ErrorIs(t, err, context.Canceled)
}

func TestAcceleratedSearchFallsBackWhenAcceleratorIsStale(t *testing.T) {
	backend, generationID, _ := openAcceleratedSearchBackend(t, 2, 4, Options{})
	require.NoError(t, backend.Upsert(t.Context(), generationID, []vector.Chunk{
		{MessageID: 1, Vector: unitVec(4, 0)},
		{MessageID: 2, Vector: unitVec(4, 1)},
	}))
	installReadyFlatAccelerator(t, backend, generationID, 4)
	_, err := backend.db.Exec(`UPDATE vector_accelerators SET state = 'stale' WHERE generation_id = ?`,
		int64(generationID))
	require.NoError(t, err)

	hits, meta, err := backend.SearchWithMetadata(t.Context(), generationID,
		unitVec(4, 0), 1, vector.Filter{})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, int64(1), hits[0].MessageID)
	assert.Equal(t, "exact", meta.Accelerator)
	assert.True(t, meta.PoolSaturated)
}

func TestAcceleratedSearchUsesTrainedVec1Index(t *testing.T) {
	backend, generationID, _ := openAcceleratedSearchBackend(t, 512, 8, Options{})
	chunks := make([]vector.Chunk, 0, 512)
	for i := range 512 {
		chunks = append(chunks, vector.Chunk{
			MessageID: int64(i + 1), Vector: unitVec(8, i%8),
		})
	}
	require.NoError(t, backend.Upsert(t.Context(), generationID, chunks))
	plan, err := backend.PrepareAccelerator(t.Context(), generationID, OptimizeOptions{Threads: 1})
	require.NoError(t, err)
	require.True(t, plan.Applicable)
	require.NoError(t, RunAcceleratorWorker(t.Context(), backend.path, generationID, 1))
	_, err = backend.PublishAccelerator(t.Context(), generationID)
	require.NoError(t, err)

	hits, meta, err := backend.SearchWithMetadata(t.Context(), generationID,
		unitVec(8, 3), 5, vector.Filter{})
	require.NoError(t, err)
	require.Len(t, hits, 5)
	assert.Equal(t, acceleratorKind, meta.Accelerator)
	for _, hit := range hits {
		assert.Equal(t, 3, int(hit.MessageID-1)%8)
		assert.InDelta(t, 1, hit.Score, 0.0001)
	}
}

// TestAcceleratedSearchWidensNProbeForSelectiveFilter exercises the ANN rerank
// and widening-under-filter branch on a trained index. The injected threshold
// keeps the filtered population above the exact-filter cutoff without a
// 32k-vector corpus. The filter matches fewer messages than k, so the search
// must widen pool and nprobe to exhaustion and still agree with exact search.
func TestAcceleratedSearchWidensNProbeForSelectiveFilter(t *testing.T) {
	backend, generationID, mainDB := openAcceleratedSearchBackend(t, 4096, 8, Options{ANNWorkCeiling: 512})
	chunks := make([]vector.Chunk, 0, 4096)
	tx, err := mainDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	for i := 1; i <= 4096; i++ {
		chunks = append(chunks, vector.Chunk{MessageID: int64(i), Vector: clusteredTestVector(int64(i), 8)})
		if i <= 11 {
			_, err = tx.Exec(`UPDATE messages SET source_id = 3 WHERE id = ?`, i)
			require.NoError(t, err)
		}
	}
	require.NoError(t, tx.Commit())
	require.NoError(t, backend.Upsert(t.Context(), generationID, chunks))
	plan, err := backend.PrepareAccelerator(t.Context(), generationID, OptimizeOptions{Threads: 1})
	require.NoError(t, err)
	require.True(t, plan.Applicable)
	require.NoError(t, RunAcceleratorWorker(t.Context(), backend.path, generationID, 1))
	_, err = backend.PublishAccelerator(t.Context(), generationID)
	require.NoError(t, err)
	backend.exactFilterVectorThreshold = 10

	query := clusteredTestVector(13, 8)
	filter := vector.Filter{SourceIDs: []int64{3}}
	exactHits, err := backend.searchExact(t.Context(), generationID, query, 12, filter)
	require.NoError(t, err)
	require.Len(t, exactHits, 11)
	annHits, metadata, err := backend.SearchWithMetadata(t.Context(), generationID, query, 12, filter)
	require.NoError(t, err)
	require.Len(t, annHits, 11)
	assert.Equal(t, acceleratorKind, metadata.Accelerator)
	assert.True(t, metadata.PoolSaturated)
	for i := range exactHits {
		assert.Equal(t, exactHits[i].MessageID, annHits[i].MessageID)
		assert.InDelta(t, exactHits[i].Score, annHits[i].Score, 0.0001)
	}
}

func TestAcceleratedSearchServesMessageIDFilterOverANNPastThreshold(t *testing.T) {
	backend, generationID, _ := openAcceleratedSearchBackend(t, 60, 4, Options{})
	chunks := make([]vector.Chunk, 0, 60)
	for i := range 60 {
		chunks = append(chunks, vector.Chunk{
			MessageID: int64(i + 1), Vector: []float32{1, float32(i+1) / 1000, 0, 0},
		})
	}
	require.NoError(t, backend.Upsert(t.Context(), generationID, chunks))
	installReadyFlatAccelerator(t, backend, generationID, 4)
	backend.exactFilterVectorThreshold = 10

	requested := make([]int64, 0, 30)
	for id := int64(21); id <= 50; id++ {
		requested = append(requested, id)
	}
	filter := vector.Filter{MessageIDs: requested}
	query := unitVec(4, 0)
	exactHits, err := backend.searchExact(t.Context(), generationID, query, 5, filter)
	require.NoError(t, err)
	require.Len(t, exactHits, 5)

	hits, metadata, err := backend.SearchWithMetadata(t.Context(), generationID, query, 5, filter)
	require.NoError(t, err)
	require.Len(t, hits, 5)
	assert.Equal(t, acceleratorKind, metadata.Accelerator,
		"an explicit MessageIDs filter above the exact-filter threshold must run on the ANN accelerator")
	for i := range exactHits {
		assert.Equal(t, exactHits[i].MessageID, hits[i].MessageID)
		assert.InDelta(t, exactHits[i].Score, hits[i].Score, 0.0001)
	}
	assert.True(t, metadata.PoolSaturated)
}

func openAcceleratedSearchBackend(
	t *testing.T,
	messageCount int,
	dimension int,
	overrides Options,
) (*Backend, vector.GenerationID, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.db")
	mainDB, err := sql.Open("sqlite3", mainPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, mainDB.Close()) })
	_, err = mainDB.Exec(`CREATE TABLE messages (
		id INTEGER PRIMARY KEY,
		source_id INTEGER,
		conversation_id INTEGER,
		message_type TEXT NOT NULL DEFAULT 'email',
		deleted_at DATETIME,
		deleted_from_source_at DATETIME
	)`)
	require.NoError(t, err)
	tx, err := mainDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	for i := 1; i <= messageCount; i++ {
		_, err := tx.Exec(`INSERT INTO messages (id, source_id, conversation_id)
			VALUES (?, ?, ?)`, i, i, i)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())

	overrides.Path = filepath.Join(dir, "vectors.db")
	overrides.MainPath = mainPath
	overrides.MainDB = mainDB
	overrides.Dimension = dimension
	backend, err := Open(t.Context(), overrides)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, backend.Close()) })
	generationID, err := backend.CreateGeneration(t.Context(), "model", dimension, "model:test")
	require.NoError(t, err)
	return backend, generationID, mainDB
}

func TestAcceleratedSearchLogsFailureAndReturnsExactResults(t *testing.T) {
	backend, gen, _ := openAcceleratedSearchBackend(t, 2, 4, Options{})
	require.NoError(t, backend.Upsert(t.Context(), gen, []vector.Chunk{
		{MessageID: 1, Vector: unitVec(4, 0)}, {MessageID: 2, Vector: unitVec(4, 1)},
	}))
	installReadyFlatAccelerator(t, backend, gen, 4)
	_, err := backend.db.Exec(`DROP TABLE ` + acceleratorTableName(gen))
	require.NoError(t, err)
	_, err = backend.db.Exec(`CREATE TABLE ` + acceleratorTableName(gen) + ` (embedding BLOB)`)
	require.NoError(t, err)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	hits, meta, err := backend.SearchWithMetadata(t.Context(), gen, unitVec(4, 0), 2, vector.Filter{})
	require.NoError(t, err)
	require.Len(t, hits, 2)
	assert.Equal(t, int64(1), hits[0].MessageID)
	assert.Equal(t, "exact-fallback", meta.Accelerator)
	assert.Contains(t, logs.String(), "level=WARN")
	assert.Contains(t, logs.String(), "query Vec1 accelerator")
}

func TestAcceleratedSearchHonorsLimitAboveWorkCeiling(t *testing.T) {
	backend, gen, _ := openAcceleratedSearchBackend(t, 80, 4, Options{ANNWorkCeiling: 32})
	chunks := make([]vector.Chunk, 80)
	for i := range chunks {
		chunks[i] = vector.Chunk{MessageID: int64(i + 1), Vector: unitVec(4, i%4)}
	}
	require.NoError(t, backend.Upsert(t.Context(), gen, chunks))
	installReadyFlatAccelerator(t, backend, gen, 4)
	hits, meta, err := backend.SearchWithMetadata(t.Context(), gen, unitVec(4, 0), 65, vector.Filter{})
	require.NoError(t, err)
	assert.Len(t, hits, 65)
	assert.Equal(t, "exact", meta.Accelerator)
}

func clusteredTestVector(ordinal int64, dimension int) []float32 {
	clusterState := uint64(ordinal/64) + 0x9e3779b97f4a7c15
	pointState := uint64(ordinal) + 0xd1b54a32d192ed03
	result := make([]float32, dimension)
	var norm float64
	for i := range result {
		clusterState = clusteredTestXorshift(clusterState)
		pointState = clusteredTestXorshift(pointState)
		base := float64(int32(clusterState>>32)) / float64(math.MaxInt32)
		jitter := float64(int32(pointState>>32)) / float64(math.MaxInt32)
		value := base + 0.025*jitter
		result[i] = float32(value)
		norm += value * value
	}
	scale := float32(1 / math.Sqrt(norm))
	for i := range result {
		result[i] *= scale
	}
	return result
}

func clusteredTestXorshift(value uint64) uint64 {
	value ^= value << 13
	value ^= value >> 7
	value ^= value << 17
	return value
}
