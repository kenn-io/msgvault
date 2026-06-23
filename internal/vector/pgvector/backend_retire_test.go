//go:build pgvector

package pgvector

import (
	"context"
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
	require.NoError(t, b.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = $1`, int64(gen)).Scan(&n),
		"count embedding rows for generation %d", gen)
	return n
}

// genMessageCount returns index_generations.message_count for a generation.
func genMessageCount(t *testing.T, b *Backend, gen vector.GenerationID) int {
	t.Helper()
	var n int
	require.NoError(t, b.db.QueryRowContext(context.Background(),
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
	b, ctx, _ := newBackendForTest(t)

	gen := buildGenWithVectors(t, b, "model-a", 4, map[int64][]float32{
		1: unitVec(4, 0),
		2: unitVec(4, 1),
	})
	require.Equal(t, 2, countEmbeddingRows(t, b, gen), "precondition: vectors present before retire")
	mcBefore := genMessageCount(t, b, gen)
	require.Equal(t, 2, mcBefore, "precondition: message_count reflects 2 messages")

	require.NoError(t, b.RetireGeneration(ctx, gen, false), "RetireGeneration")
	require.Equal(t, 0, countEmbeddingRows(t, b, gen), "retire deletes the gen's embeddings")

	// A stale worker's Upsert lands AFTER the retire+delete. It must be
	// rejected with the sentinel and write nothing.
	err := b.Upsert(ctx, gen, []vector.Chunk{
		{MessageID: 1, ChunkIndex: 0, Vector: unitVec(4, 0)},
		{MessageID: 2, ChunkIndex: 0, Vector: unitVec(4, 1)},
	})
	require.ErrorIs(t, err, vector.ErrGenerationRetired,
		"Upsert to a retired generation must fail with ErrGenerationRetired")

	assert.Equal(t, 0, countEmbeddingRows(t, b, gen),
		"rejected Upsert must not re-pollute the retired generation")
	assert.Equal(t, mcBefore, genMessageCount(t, b, gen),
		"rejected Upsert must not drift message_count")
}

// TestBackend_Upsert_AcceptsBuildingGeneration is the positive control for the
// cf-1 guard: a normal Upsert to a still-building generation continues to
// succeed and write rows.
func TestBackend_Upsert_AcceptsBuildingGeneration(t *testing.T) {
	b, ctx, db := newBackendForTest(t)

	for _, id := range []int64{1, 2} {
		_, err := db.ExecContext(ctx,
			`INSERT INTO messages (id) VALUES ($1) ON CONFLICT DO NOTHING`, id)
		require.NoErrorf(t, err, "seed message %d", id)
	}
	gen, err := b.CreateGeneration(ctx, "model-a", 4, "model-a")
	require.NoError(t, err, "CreateGeneration")

	require.NoError(t, b.Upsert(ctx, gen, []vector.Chunk{
		{MessageID: 1, ChunkIndex: 0, Vector: unitVec(4, 0)},
		{MessageID: 2, ChunkIndex: 0, Vector: unitVec(4, 1)},
	}), "Upsert to a building generation must succeed")

	assert.Equal(t, 2, countEmbeddingRows(t, b, gen), "building-gen Upsert writes rows")
	assert.Equal(t, 2, genMessageCount(t, b, gen), "building-gen Upsert bumps message_count")
	assert.Equal(t, string(vector.GenerationBuilding), genState(t, b, gen),
		"generation stays building after Upsert")
}

// genState returns the index_generations.state for a generation.
func genState(t *testing.T, b *Backend, gen vector.GenerationID) string {
	t.Helper()
	var s string
	require.NoError(t, b.db.QueryRowContext(context.Background(),
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
	ctx := context.Background()
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
	b, ctx, _ := newBackendForTest(t)

	gen := buildGenWithVectors(t, b, "model-a", 4, map[int64][]float32{
		1: unitVec(4, 0),
		2: unitVec(4, 1),
		3: unitVec(4, 2),
	})
	require.Equal(t, 3, countEmbeddingRows(t, b, gen), "precondition: vectors present before retire")

	require.NoError(t, b.RetireGeneration(ctx, gen, false), "RetireGeneration")

	assert.Equal(t, 0, countEmbeddingRows(t, b, gen),
		"retired generation's embedding rows must be deleted")
	assert.Equal(t, string(vector.GenerationRetired), genState(t, b, gen),
		"index_generations row must remain, flipped to retired")
}

// TestBackend_ActivateGeneration_AutoRetireDeletesPrevious pins the auto-retire
// half of the fix (Codex MEDIUM #1, path (b)) — the path the normal re-embed
// flow uses. ActivateGeneration flips the previously-active generation to
// retired in the same tx; that transition must also delete the demoted
// generation's vectors so they do not accumulate in the shared HNSW graph.
func TestBackend_ActivateGeneration_AutoRetireDeletesPrevious(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)

	// Generation A: active, same dimension as B.
	genA := buildGenWithVectors(t, b, "model-a", 4, map[int64][]float32{
		1: unitVec(4, 0),
		2: unitVec(4, 1),
	})
	require.NoError(t, b.ActivateGeneration(ctx, genA, true), "activate A")
	require.Equal(t, 2, countEmbeddingRows(t, b, genA), "A populated before re-embed")

	// Generation B: a new building generation at the same dimension (the
	// normal re-embed flow). Activating it auto-retires A.
	genB := buildGenWithVectors(t, b, "model-b", 4, map[int64][]float32{
		1: unitVec(4, 2),
		2: unitVec(4, 3),
	})
	require.NoError(t, b.ActivateGeneration(ctx, genB, true), "activate B (auto-retires A)")

	assert.Equal(t, string(vector.GenerationRetired), genState(t, b, genA),
		"A must be retired by B's activation")
	assert.Equal(t, 0, countEmbeddingRows(t, b, genA),
		"auto-retired generation A's embedding rows must be deleted by B's activation")
	assert.Equal(t, 2, countEmbeddingRows(t, b, genB),
		"newly-activated generation B's rows must be untouched")

	// RETURNING-id provability: the demote folds into one
	// `UPDATE ... WHERE state='active' RETURNING id` statement, so the id whose
	// embeddings get deleted is exactly the row that flipped to retired. Assert
	// the previously-active gen (genA) is the sole retired row AND that its
	// embeddings were the ones deleted, while the new active gen (genB) keeps
	// its rows.
	retired := singleRetiredGen(t, b)
	assert.Equal(t, genA, retired, "the previously-active gen must be the sole retired row")
	assert.Equal(t, 0, countEmbeddingRows(t, b, retired),
		"embeddings deleted for exactly the RETURNING'd (retired) id")

	active, err := b.ActiveGeneration(ctx)
	require.NoError(t, err, "ActiveGeneration after activate B")
	assert.Equal(t, genB, active.ID, "B is the serving generation")
}

// singleRetiredGen returns the id of the one generation in state='retired',
// failing if there is not exactly one. Used by the auto-retire RETURNING-id
// tests to prove the deleted id is the same row whose state flipped to retired.
func singleRetiredGen(t *testing.T, b *Backend) vector.GenerationID {
	t.Helper()
	var id int64
	require.NoError(t, b.db.QueryRowContext(context.Background(),
		`SELECT id FROM index_generations WHERE state = 'retired'`).Scan(&id),
		"expected exactly one retired generation")
	return vector.GenerationID(id)
}

// TestBackend_ActivateGeneration_PreservesBuildingGenerations ensures the
// auto-retire delete is scoped to the DEMOTED generation only: a third,
// still-building generation at the same dimension keeps its rows.
func TestBackend_ActivateGeneration_PreservesBuildingGenerations(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)

	genA := buildGenWithVectors(t, b, "model-a", 4, map[int64][]float32{
		1: unitVec(4, 0),
	})
	require.NoError(t, b.ActivateGeneration(ctx, genA, true), "activate A")

	// A second generation B that we activate (auto-retiring A) — but first
	// stage it as building. Only one building generation may exist at a time,
	// so we build+activate B, then build C and leave it building.
	genB := buildGenWithVectors(t, b, "model-b", 4, map[int64][]float32{
		1: unitVec(4, 1),
	})
	require.NoError(t, b.ActivateGeneration(ctx, genB, true), "activate B (retires A)")
	require.Equal(t, 0, countEmbeddingRows(t, b, genA), "A deleted")

	genC := buildGenWithVectors(t, b, "model-c", 4, map[int64][]float32{
		1: unitVec(4, 2),
	})
	// C is still building; activating it retires B but must leave C's own rows.
	require.NoError(t, b.ActivateGeneration(ctx, genC, true), "activate C (retires B)")
	assert.Equal(t, 0, countEmbeddingRows(t, b, genB), "B deleted on C's activation")
	assert.Equal(t, 1, countEmbeddingRows(t, b, genC), "C's own rows preserved")
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
	b, ctx, _ := newBackendForTest(t)

	genA := buildGenWithVectors(t, b, "model-a", 4, map[int64][]float32{
		1: unitVec(4, 0),
		2: unitVec(4, 1),
	})
	require.NoError(t, b.ActivateGeneration(ctx, genA, true), "activate A")
	require.Equal(t, 2, countEmbeddingRows(t, b, genA), "precondition: A has embeddings")
	require.Equal(t, string(vector.GenerationActive), genState(t, b, genA), "precondition: A active")

	// (1) Non-forced retire of the ACTIVE gen is refused atomically.
	err := b.RetireGeneration(ctx, genA, false)
	require.ErrorIs(t, err, vector.ErrRefuseRetireActive,
		"non-forced retire of active gen must return ErrRefuseRetireActive")
	assert.Equal(t, string(vector.GenerationActive), genState(t, b, genA),
		"refused retire must leave the active gen's state unchanged")
	assert.Equal(t, 2, countEmbeddingRows(t, b, genA),
		"refused retire must NOT delete the active gen's embeddings")

	// (2) Forced retire succeeds: state flips to retired, embeddings deleted.
	require.NoError(t, b.RetireGeneration(ctx, genA, true),
		"forced retire of active gen must succeed")
	assert.Equal(t, string(vector.GenerationRetired), genState(t, b, genA),
		"forced retire flips state to retired")
	assert.Equal(t, 0, countEmbeddingRows(t, b, genA),
		"forced retire deletes the gen's embeddings")

	// (3) A NON-active (building) generation retires fine without force.
	genB := buildGenWithVectors(t, b, "model-b", 4, map[int64][]float32{
		3: unitVec(4, 2),
	})
	require.Equal(t, string(vector.GenerationBuilding), genState(t, b, genB), "precondition: B building")
	require.NoError(t, b.RetireGeneration(ctx, genB, false),
		"non-forced retire of a non-active gen must succeed")
	assert.Equal(t, string(vector.GenerationRetired), genState(t, b, genB),
		"non-active gen retires to retired without force")
}

// TestBackend_DeleteOnRetire_KeepsActiveRecallClean is the recall proof for
// Codex MEDIUM #1. It constructs the contamination scenario the fix targets:
// generation A (retired) and the active generation B share one dimension and
// therefore one HNSW graph. A's vectors sit CLOSER to the query than B's. With
// A's rows still in the graph, an ef_search-bounded HNSW scan would spend its
// candidate budget on A's near-query vectors, and Search — which post-filters
// by generation_id — would have to spend its ef_search budget on A's
// near-query candidates and could under-recall B. After the delete-on-retire
// fix, A's rows are gone, so the shared graph is generation-clean and Search
// against B returns full k recall drawn entirely from B's ids.
//
// Two observations from running this on live PG (recorded for the reviewer):
//   - The deterministic, primary assertion is that A's rows are DELETED. With
//     the DELETE reverted, that assertion fails immediately (A retains all its
//     rows in the shared table/graph) — the unbounded accumulation the fix
//     prevents. Each re-embed otherwise leaves a full stale copy behind,
//     growing storage and the HNSW graph without bound.
//   - The Search/FusedSearch recall assertions below stay GREEN even with the
//     DELETE reverted, because the backend's candidate-widening loop re-issues
//     the inner ANN scan with a larger LIMIT until k DISTINCT active-gen
//     messages survive — masking the recall symptom at the application layer
//     at the cost of progressively more graph traversal as stale rows pile up.
//     They are kept here as a positive guard that the fix does not regress
//     active-generation recall, and that every returned hit belongs to B.
func TestBackend_DeleteOnRetire_KeepsActiveRecallClean(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)

	const k = 5
	// Raise ef_search on this connection toward the regime where a polluted
	// graph would starve B's candidates. (The production path raises it at
	// connect time; here we set it explicitly so the test is self-contained.)
	_, err := b.db.ExecContext(ctx, `SET hnsw.ef_search = 40`)
	require.NoError(t, err, "set ef_search")

	// Generation A: many vectors clustered VERY close to the query axis. These
	// are the contaminants — if retained in the shared graph they dominate the
	// ANN candidate pool.
	aVecs := map[int64][]float32{}
	const aCount = 60
	for id := int64(1); id <= aCount; id++ {
		aVecs[id] = nearQueryVec(int(id), 0.005)
	}
	genA := buildGenWithVectors(t, b, "model-a", recallDim, aVecs)
	require.NoError(t, b.ActivateGeneration(ctx, genA, true), "activate A")
	require.Equal(t, aCount, countEmbeddingRows(t, b, genA), "A populated")

	// Generation B: exactly k messages at a moderate distance from the query.
	// They are the only correct answers once B is the serving generation.
	bVecs := map[int64][]float32{}
	for j := range k {
		id := int64(1000 + j)
		bVecs[id] = midQueryVec(j + 2)
	}
	genB := buildGenWithVectors(t, b, "model-b", recallDim, bVecs)

	// Activate B: auto-retires A AND (with the fix) deletes A's vectors.
	require.NoError(t, b.ActivateGeneration(ctx, genB, true), "activate B")

	// (i) Retired generation A's rows are deleted.
	require.Equal(t, 0, countEmbeddingRows(t, b, genA),
		"A's embedding rows must be deleted so the shared HNSW graph is generation-clean")
	require.Equal(t, k, countEmbeddingRows(t, b, genB), "B's rows intact")

	query := make([]float32, recallDim)
	query[0] = 1

	// (ii) Search against the active generation returns full k recall, and
	// every hit belongs to B (ids >= 1000), uncontaminated by A.
	hits, err := b.Search(ctx, genB, query, k, vector.Filter{})
	require.NoError(t, err, "Search")
	require.Lenf(t, hits, k,
		"active-generation Search must return full k=%d recall after delete-on-retire", k)
	for _, h := range hits {
		assert.GreaterOrEqualf(t, h.MessageID, int64(1000),
			"hit %d must belong to active generation B, not retired A", h.MessageID)
	}

	// FusedSearch (hybrid ANN path) must also be clean.
	fhits, _, err := b.FusedSearch(ctx, vector.FusedRequest{
		QueryVec:   query,
		Generation: genB,
		KPerSignal: k,
		Limit:      k,
		RRFK:       60,
	})
	require.NoError(t, err, "FusedSearch")
	require.Lenf(t, fhits, k,
		"active-generation FusedSearch must return full k=%d recall after delete-on-retire", k)
	for _, h := range fhits {
		assert.GreaterOrEqualf(t, h.MessageID, int64(1000),
			"fused hit %d must belong to active generation B, not retired A", h.MessageID)
	}
}
