//go:build pgvector

package pgvector

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

// countEmbeddingRows returns the number of embedding rows belonging to a
// generation. Used by the delete-on-retire tests to assert that a retired
// generation's vectors are removed from the shared HNSW graph.
func countEmbeddingRows(t *testing.T, b *Backend, gen vector.GenerationID) int {
	t.Helper()
	var n int
	require.NoError(t, b.db.QueryRowContext(
		t.Context(),
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = $1`, int64(gen)).Scan(&n),
		"count embedding rows for generation %d", gen)
	return n
}

// genMessageCount returns index_generations.message_count for a generation.
func genMessageCount(t *testing.T, b *Backend, gen vector.GenerationID) int {
	t.Helper()
	var n int
	require.NoError(t, b.db.QueryRowContext(
		t.Context(),
		`SELECT message_count FROM index_generations WHERE id = $1`, int64(gen)).Scan(&n),
		"read message_count for generation %d", gen)
	return n
}

// TestBackend_Upsert_RejectsRetiredGeneration pins the cf-1 race guard. After a
// generation is retired (which deletes its embeddings so the shared HNSW graph
// stays generation-clean), a stale worker — or `embeddings retire
// --force-active` racing in-flight claims — must NOT be able to re-insert
// vectors into it. Upsert now reads dimension+state under FOR UPDATE inside the
// write tx and aborts with ErrGenerationRetired, leaving zero rows and an
// unchanged message_count.
func TestBackend_Upsert_RejectsRetiredGeneration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	b, ctx, _ := newBackendForTest(t)

	gen := buildGenWithVectors(t, b, "model-a", 4, map[int64][]float32{
		1: unitVec(4, 0),
		2: unitVec(4, 1),
	})
	require.Equal(2, countEmbeddingRows(t, b, gen), "precondition: vectors present before retire")
	mcBefore := genMessageCount(t, b, gen)
	require.Equal(2, mcBefore, "precondition: message_count reflects 2 messages")
	require.NoError(
		b.RetireGeneration(ctx, gen, false), "RetireGeneration")

	require.Equal(0, countEmbeddingRows(t, b, gen), "retire deletes the gen's embeddings")

	// A stale worker's Upsert lands AFTER the retire+delete. It must be
	// rejected with the sentinel and write nothing.
	err := b.Upsert(ctx, gen, []vector.Chunk{
		{MessageID: 1, ChunkIndex: 0, Vector: unitVec(4, 0)},
		{MessageID: 2, ChunkIndex: 0, Vector: unitVec(4, 1)},
	})
	require.ErrorIs(err, vector.ErrGenerationRetired,
		"Upsert to a retired generation must fail with ErrGenerationRetired")
	assert.Equal(0, countEmbeddingRows(t, b, gen),
		"rejected Upsert must not re-pollute the retired generation")
	assert.Equal(mcBefore, genMessageCount(t, b, gen),
		"rejected Upsert must not drift message_count")
}

// TestBackend_Upsert_AcceptsBuildingGeneration is the positive control for the
// cf-1 guard: a normal Upsert to a still-building generation continues to
// succeed and write rows.
func TestBackend_Upsert_AcceptsBuildingGeneration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	b, ctx, db := newBackendForTest(t)

	for _, id := range []int64{1, 2} {
		_, err := db.ExecContext(ctx,
			`INSERT INTO messages (id) VALUES ($1) ON CONFLICT DO NOTHING`, id)
		require.NoErrorf(err, "seed message %d", id)
	}
	gen, err := b.CreateGeneration(ctx, "model-a", 4, "model-a")
	require.NoError(
		err, "CreateGeneration")

	require.NoError(
		b.Upsert(ctx, gen, []vector.Chunk{
			{MessageID: 1, ChunkIndex: 0, Vector: unitVec(4, 0)},
			{MessageID: 2, ChunkIndex: 0, Vector: unitVec(4, 1)},
		}), "Upsert to a building generation must succeed")

	assert.Equal(2, countEmbeddingRows(t, b, gen), "building-gen Upsert writes rows")
	assert.Equal(2, genMessageCount(t, b, gen), "building-gen Upsert bumps message_count")
	assert.Equal(string(vector.GenerationBuilding), genState(t, b, gen),
		"generation stays building after Upsert")
}

// genState returns the index_generations.state for a generation.
func genState(t *testing.T, b *Backend, gen vector.GenerationID) string {
	t.Helper()
	var s string
	require.NoError(t, b.db.QueryRowContext(
		t.Context(),
		`SELECT state FROM index_generations WHERE id = $1`, int64(gen)).Scan(&s),
		"read state for generation %d", gen)
	return s
}

// buildGenWithVectors creates a fresh building generation, upserts one chunk
// per supplied (message_id -> vector), stamps embed_gen on those messages
// (fully-embedded end state), and returns the gen id. It does NOT activate.
// Caller controls activation order so the retire paths can be exercised
// explicitly.
func buildGenWithVectors(t *testing.T, b *Backend, model string, dim int, vecs map[int64][]float32) vector.GenerationID {
	t.Helper()
	ctx := t.Context()
	for id := range vecs {
		_, err := b.db.ExecContext(ctx,
			`INSERT INTO messages (id) VALUES ($1) ON CONFLICT DO NOTHING`, id)
		require.NoErrorf(t, err, "seed message %d", id)
	}
	gen, err := b.CreateGeneration(ctx, model, dim, model)
	require.NoError(t, err, "CreateGeneration")
	chunks := make([]vector.Chunk, 0, len(vecs))
	for id, v := range vecs {
		chunks = append(chunks, vector.Chunk{MessageID: id, ChunkIndex: 0, Vector: v})
	}
	require.NoError(t, b.Upsert(ctx, gen, chunks), "Upsert")
	for id := range vecs {
		_, err = b.db.ExecContext(ctx,
			`UPDATE messages SET embed_gen = $1 WHERE id = $2`, int64(gen), id)
		require.NoErrorf(t, err, "stamp embed_gen for msg %d", id)
	}
	return gen
}

// TestBackend_RetireGeneration_DeletesEmbeddings pins the explicit-retire half
// of the delete-on-retire fix (Codex MEDIUM #1, path (a)). pgvector's HNSW
// index is partial by DIMENSION only, so all generations of a dimension share
// one graph and Search post-filters by generation_id. RetireGeneration must
// therefore delete the retired generation's vectors so they cannot consume the
// shared graph's ef_search budget. The index_generations row stays (so history
// and lifecycle queries still see it); only its embeddings are removed.
func TestBackend_RetireGeneration_DeletesEmbeddings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	b, ctx, _ := newBackendForTest(t)

	gen := buildGenWithVectors(t, b, "model-a", 4, map[int64][]float32{
		1: unitVec(4, 0),
		2: unitVec(4, 1),
		3: unitVec(4, 2),
	})
	require.Equal(3, countEmbeddingRows(t, b, gen), "precondition: vectors present before retire")
	require.NoError(
		b.RetireGeneration(ctx, gen, false), "RetireGeneration")

	assert.Equal(0, countEmbeddingRows(t, b, gen),
		"retired generation's embedding rows must be deleted")
	assert.Equal(string(vector.GenerationRetired), genState(t, b, gen),
		"index_generations row must remain, flipped to retired")
}

// TestBackend_ActivateGeneration_AutoRetireDeletesPrevious pins the auto-retire
// half of the fix (Codex MEDIUM #1, path (b)) — the path the normal re-embed
// flow uses. ActivateGeneration flips the previously-active generation to
// retired in the same tx; that transition must also delete the demoted
// generation's vectors so they do not accumulate in the shared HNSW graph.
func TestBackend_ActivateGeneration_AutoRetireDeletesPrevious(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	b, ctx, _ := newBackendForTest(t)

	// Generation A: active, same dimension as B.
	genA := buildGenWithVectors(t, b, "model-a", 4, map[int64][]float32{
		1: unitVec(4, 0),
		2: unitVec(4, 1),
	})
	require.NoError(
		b.ActivateGeneration(ctx, genA, true), "activate A")

	require.Equal(2, countEmbeddingRows(t, b, genA), "A populated before re-embed")

	// Generation B: a new building generation at the same dimension (the
	// normal re-embed flow). Activating it auto-retires A.
	genB := buildGenWithVectors(t, b, "model-b", 4, map[int64][]float32{
		1: unitVec(4, 2),
		2: unitVec(4, 3),
	})
	require.NoError(
		b.ActivateGeneration(ctx, genB, true), "activate B (auto-retires A)")

	assert.Equal(string(vector.GenerationRetired), genState(t, b, genA),
		"A must be retired by B's activation")
	assert.Equal(0, countEmbeddingRows(t, b, genA),
		"auto-retired generation A's embedding rows must be deleted by B's activation")
	assert.Equal(2, countEmbeddingRows(t, b, genB),
		"newly-activated generation B's rows must be untouched")

	// RETURNING-id provability: the demote folds into one
	// `UPDATE ... WHERE state='active' RETURNING id` statement, so the id whose
	// embeddings get deleted is exactly the row that flipped to retired. Assert
	// the previously-active gen (genA) is the sole retired row AND that its
	// embeddings were the ones deleted, while the new active gen (genB) keeps
	// its rows.
	retired := singleRetiredGen(t, b)
	assert.Equal(genA, retired, "the previously-active gen must be the sole retired row")
	assert.Equal(0, countEmbeddingRows(t, b, retired),
		"embeddings deleted for exactly the RETURNING'd (retired) id")

	active, err := b.ActiveGeneration(ctx)
	require.NoError(
		err, "ActiveGeneration after activate B")

	assert.Equal(genB, active.ID, "B is the serving generation")
}

// singleRetiredGen returns the id of the one generation in state='retired',
// failing if there is not exactly one. Used by the auto-retire RETURNING-id
// tests to prove the deleted id is the same row whose state flipped to retired.
func singleRetiredGen(t *testing.T, b *Backend) vector.GenerationID {
	t.Helper()
	var id int64
	require.NoError(t, b.db.QueryRowContext(
		t.Context(),
		`SELECT id FROM index_generations WHERE state = 'retired'`).Scan(&id),
		"expected exactly one retired generation")
	return vector.GenerationID(id)
}

// TestBackend_ActivateGeneration_PreservesBuildingGenerations ensures the
// auto-retire delete is scoped to the DEMOTED generation only: a third,
// still-building generation at the same dimension keeps its rows.
func TestBackend_ActivateGeneration_PreservesBuildingGenerations(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	b, ctx, _ := newBackendForTest(t)

	genA := buildGenWithVectors(t, b, "model-a", 4, map[int64][]float32{
		1: unitVec(4, 0),
	})
	require.NoError(
		b.ActivateGeneration(ctx, genA, true), "activate A")

	// A second generation B that we activate (auto-retiring A) — but first
	// stage it as building. Only one building generation may exist at a time,
	// so we build+activate B, then build C and leave it building.
	genB := buildGenWithVectors(t, b, "model-b", 4, map[int64][]float32{
		1: unitVec(4, 1),
	})
	require.NoError(
		b.ActivateGeneration(ctx, genB, true), "activate B (retires A)")

	require.Equal(0, countEmbeddingRows(t, b, genA), "A deleted")

	genC := buildGenWithVectors(t, b, "model-c", 4, map[int64][]float32{
		1: unitVec(4, 2),
	})
	require.NoError(

		b.ActivateGeneration(ctx, genC, true), "activate C (retires B)")

	assert.Equal(0, countEmbeddingRows(t, b, genB), "B deleted on C's activation")
	assert.Equal(1, countEmbeddingRows(t, b, genC), "C's own rows preserved")
}

// TestBackend_RetireGeneration_ActiveGuard pins the retire-TOCTOU
// class-closing fix: the active-gen guard lives ATOMICALLY inside
// RetireGeneration's tx (mirroring ActivateGeneration's force gate). On
// pgvector, retire DELETES the generation's embeddings (shared HNSW graph),
// so refusing the active generation without force is what prevents a
// concurrent activation from wiping the now-serving graph.
//   - force=false against the ACTIVE generation is refused with
//     ErrRefuseRetireActive, leaving state='active' and its embeddings intact.
//   - force=true retires the active generation, deleting embeddings.
//   - force=false against a NON-active (building) generation retires fine.
func TestBackend_RetireGeneration_ActiveGuard(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	b, ctx, _ := newBackendForTest(t)

	genA := buildGenWithVectors(t, b, "model-a", 4, map[int64][]float32{
		1: unitVec(4, 0),
		2: unitVec(4, 1),
	})
	require.NoError(
		b.ActivateGeneration(ctx, genA, true), "activate A")

	require.Equal(2, countEmbeddingRows(t, b, genA), "precondition: A has embeddings")
	require.Equal(string(vector.GenerationActive), genState(t, b, genA), "precondition: A active")

	// (1) Non-forced retire of the ACTIVE gen is refused atomically.
	err := b.RetireGeneration(ctx, genA, false)
	require.ErrorIs(err, vector.ErrRefuseRetireActive,
		"non-forced retire of active gen must return ErrRefuseRetireActive")
	assert.Equal(string(vector.GenerationActive), genState(t, b, genA),
		"refused retire must leave the active gen's state unchanged")
	assert.Equal(2, countEmbeddingRows(t, b, genA),
		"refused retire must NOT delete the active gen's embeddings")
	require.NoError(

		b.RetireGeneration(ctx, genA, true),
		"forced retire of active gen must succeed")

	assert.Equal(string(vector.GenerationRetired), genState(t, b, genA),
		"forced retire flips state to retired")
	assert.Equal(0, countEmbeddingRows(t, b, genA),
		"forced retire deletes the gen's embeddings")

	// (3) A NON-active (building) generation retires fine without force.
	genB := buildGenWithVectors(t, b, "model-b", 4, map[int64][]float32{
		3: unitVec(4, 2),
	})
	require.Equal(string(vector.GenerationBuilding), genState(t, b, genB), "precondition: B building")
	require.NoError(
		b.RetireGeneration(ctx, genB, false),
		"non-forced retire of a non-active gen must succeed")

	assert.Equal(string(vector.GenerationRetired), genState(t, b, genB),
		"non-active gen retires to retired without force")
}

func TestBackend_DeleteOnRetire_KeepsActiveRecallClean(t *testing.T) {
	require := require.New(t)

	b, ctx, _ := newBackendForTest(t)

	const k = 5
	// Generation A populates the shared dimension index.
	aVecs := map[int64][]float32{}
	const aCount = 60
	for id := int64(1); id <= aCount; id++ {
		aVecs[id] = nearQueryVec(int(id), 0.005)
	}
	genA := buildGenWithVectors(t, b, "model-a", recallDim, aVecs)
	require.NoError(
		b.ActivateGeneration(ctx, genA, true), "activate A")

	require.Equal(aCount, countEmbeddingRows(t, b, genA), "A populated")

	// Generation B becomes the serving generation at the same dimension.
	bVecs := map[int64][]float32{}
	for j := range k {
		id := int64(1000 + j)
		bVecs[id] = midQueryVec(j + 2)
	}
	genB := buildGenWithVectors(t, b, "model-b", recallDim, bVecs)
	require.NoError(

		b.ActivateGeneration(ctx, genB, true), "activate B")

	require. // Retired generation A's rows are deleted.
			Equal(0, countEmbeddingRows(t, b, genA),
			"A's embedding rows must be deleted so the shared HNSW graph is generation-clean")
	require.Equal(k, countEmbeddingRows(t, b, genB), "B's rows intact")
}

// The building generation shares the active generation's dimension index.
// Its closer vectors must not crowd the active generation out of search.
func TestBackend_SearchDuringSameDimensionBuild(t *testing.T) {
	b, ctx, db := newBackendForTest(t)
	db.SetMaxOpenConns(1)
	_, err := db.ExecContext(ctx, `SET enable_seqscan = off; SET hnsw.ef_search = 40`)
	require.NoError(t, err)
	activeVectors := map[int64][]float32{}
	for id := int64(1); id <= 5; id++ {
		activeVectors[id] = midQueryVec(int(id))
	}
	active := buildGenWithVectors(t, b, "active", recallDim, activeVectors)
	require.NoError(t, b.ActivateGeneration(ctx, active, true))
	buildingVectors := map[int64][]float32{}
	for id := int64(1); id <= 300; id++ {
		buildingVectors[id] = nearQueryVec(int(id), 0.005)
	}
	buildGenWithVectors(t, b, "building", recallDim, buildingVectors)
	_, err = db.ExecContext(ctx, `ANALYZE embeddings`)
	require.NoError(t, err)
	query := unitVec(recallDim, 0)
	for _, filter := range []vector.Filter{{}, {MessageIDs: []int64{1, 2, 3, 4, 5}}} {
		hits, err := b.Search(ctx, active, query, 5, filter)
		require.NoError(t, err)
		require.Len(t, hits, 5)
		for _, hit := range hits {
			assert.LessOrEqual(t, hit.MessageID, int64(5))
		}
		fused, _, err := b.FusedSearch(ctx, vector.FusedRequest{QueryVec: query, Generation: active, KPerSignal: 5, Limit: 5, RRFK: 60, Filter: filter})
		require.NoError(t, err)
		require.Len(t, fused, 5)
		for _, hit := range fused {
			assert.LessOrEqual(t, hit.MessageID, int64(5))
		}
	}
}
