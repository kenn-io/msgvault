//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"math"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/vector"
)

func TestUnretrievedCosineBoundFollowsStoredVectorLengths(t *testing.T) {
	ctx := context.Background()
	main := openMainDBWithOneMessage(t)
	_, err := main.Exec(`INSERT INTO messages (id) VALUES (2), (3), (4)`)
	require.NoError(t, err)
	b, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "vectors.db"), Dimension: 768, MainDB: main})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	id, err := b.CreateGeneration(ctx, "model", 768, "")
	require.NoError(t, err)
	gen := vector.Generation{ID: id, Dimension: 768}
	vec := func(x, y float32) []float32 {
		v := make([]float32, 768)
		v[0], v[1] = x, y
		return v
	}
	query := vec(1, 0)
	// A unit vector at cosine .6 to the query lies sqrt(.8) away by L2.
	edge := 1 - math.Sqrt(.8)
	bound := func() float64 {
		t.Helper()
		got, err := b.UnretrievedCosineBound(ctx, gen, query, edge)
		require.NoError(t, err)
		return got
	}

	assert.Zero(t, bound(), "an empty generation hides nothing")
	require.NoError(t, b.Upsert(ctx, id, []vector.Chunk{
		{MessageID: 1, Vector: vec(.6, .8)},
		{MessageID: 2, Vector: vec(0, 0)},
	}))
	assert.InDelta(t, .6, bound(), 1e-3, "with unit vectors, L2 order is cosine order")
	zero, err := b.UnretrievedCosineBound(ctx, gen, vec(0, 0), edge)
	require.NoError(t, err)
	assert.Zero(t, zero, "a zero query scores zero against everything")

	require.NoError(t, b.Delete(ctx, id, []int64{2}))
	assert.InDelta(t, .6, bound(), 1e-3, "Delete keeps the range current")

	require.NoError(t, b.Upsert(ctx, id, []vector.Chunk{{MessageID: 3, Vector: vec(30, 0)}}))
	assert.InDelta(t, 1, bound(), 1e-9, "a long vector far away can still point the query's way")

	// A write that does not record lengths, such as an older binary's,
	// leaves the range stale, so the bound gives up.
	require.NoError(t, b.ActivateGeneration(ctx, id, true))
	fresh, err := b.CreateGeneration(ctx, "fresh", 768, "")
	require.NoError(t, err)
	freshGen := vector.Generation{ID: fresh, Dimension: 768}
	require.NoError(t, b.Upsert(ctx, fresh, []vector.Chunk{{MessageID: 1, Vector: vec(.6, .8)}}))
	freshBound := func() float64 {
		t.Helper()
		got, err := b.UnretrievedCosineBound(ctx, freshGen, query, edge)
		require.NoError(t, err)
		return got
	}
	require.InDelta(t, .6, freshBound(), 1e-3)
	_, err = b.db.ExecContext(ctx, `INSERT INTO embeddings
		(generation_id, message_id, chunk_index, embedded_at, source_char_len,
		 chunk_char_start, chunk_char_end, truncated, source_basis)
		VALUES (?, 4, 0, 0, 0, 0, 0, 0, 0)`, int64(fresh))
	require.NoError(t, err)
	assert.InDelta(t, 1, freshBound(), 1e-9, "a stale range bounds nothing")

	// A generation from before length tracking has no recorded range.
	_, err = b.db.ExecContext(ctx, `UPDATE index_generations
		SET vector_norm_min = NULL, vector_norm_max = NULL, vector_norm_revision = NULL WHERE id = ?`, int64(id))
	require.NoError(t, err)
	assert.InDelta(t, 1, bound(), 1e-9, "an unknown range bounds nothing")
}

func TestUnretrievedCosineBoundCoversContextualPublication(t *testing.T) {
	b, ctx := newBackendForTest(t)
	id, err := b.CreateGeneration(ctx, "model", 768, "model:768:context")
	require.NoError(t, err)
	gen := vector.Generation{ID: id, Dimension: 768}
	const scope = "chat:lengths:day"
	window := documentPublication("chat:lengths:window", "revision-a", 1, 91)
	require.NoError(t, b.PublishScope(ctx, id, scope, 1,
		[]vector.DocumentPublication{window}, []vector.Chunk{documentChunk(91, 0.1)}))
	window.Revision, window.SourceSequence = "revision-b", 2
	require.NoError(t, b.PublishScope(ctx, id, scope, 2,
		[]vector.DocumentPublication{window}, []vector.Chunk{documentChunk(91, 0.1)}))

	// Every stored vector has length 0.1 * sqrt(768). At the distance of one
	// from the unit query, the bound is that vector's own cosine.
	query := make([]float32, 768)
	query[0] = 1
	stored := documentChunk(91, 0.1).Vector
	length := norm(stored)
	cos := 0.1 / length
	edge := 1 - math.Sqrt(1+length*length-2*length*cos)
	got, err := b.UnretrievedCosineBound(ctx, gen, query, edge)
	require.NoError(t, err)
	assert.InDelta(t, cos, got, 1e-3, "publishing and replacing a scope keep the range current")
}

func TestCosineBeyondIsExactForUniformLengths(t *testing.T) {
	for _, tc := range []struct {
		name           string
		query, vectors float64
		cosine         float64
	}{
		{"unit", 1, 1, .6},
		{"uniformly long", 3, 3, .3},
		{"opposite", 1, 1, -1},
	} {
		// The L2 distance between vectors of these lengths at this cosine.
		d := math.Sqrt(tc.query*tc.query + tc.vectors*tc.vectors - 2*tc.query*tc.vectors*tc.cosine)
		got := cosineBeyond(tc.query, d, normRange{min: tc.vectors, max: tc.vectors, any: true})
		assert.InDelta(t, tc.cosine, got, 1e-9, tc.name)
	}
}
