//go:build pgvector

package pgvector

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/vector"
)

func TestMessageVectorsReturnsChunksInIndexOrder(t *testing.T) {
	b, ctx, db := newBackendForTest(t)
	for _, id := range []int64{7, 8, 9} {
		_, err := db.ExecContext(ctx, `INSERT INTO messages (id) VALUES ($1)`, id)
		require.NoError(t, err)
	}
	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(t, err)
	chunks := []vector.Chunk{{MessageID: 8, Vector: unitVec(768, 6)}}
	for i := 5; i >= 0; i-- {
		chunks = append(chunks, vector.Chunk{MessageID: 7, ChunkIndex: i, Vector: unitVec(768, i)})
	}
	require.NoError(t, b.Upsert(ctx, gid, chunks))

	got, err := b.MessageVectors(ctx, vector.Generation{ID: gid, Dimension: 768}, []int64{9, 8, 7, 7}, 4)
	require.NoError(t, err)
	assert.Equal(t, [][]float32{unitVec(768, 0), unitVec(768, 1), unitVec(768, 2), unitVec(768, 3)}, got[7])
	assert.Equal(t, [][]float32{unitVec(768, 6)}, got[8])
	assert.NotContains(t, got, int64(9))
	assert.Len(t, got, 2)
}

func TestMessageVectorsReturnsStoredValuesUnnormalized(t *testing.T) {
	b, ctx, db := newBackendForTest(t)
	v := make([]float32, 768)
	v[0], v[1] = 3, 4
	gid := seedAndEmbed(t, b, db, map[int64][]float32{1: v})
	got, err := b.MessageVectors(ctx, vector.Generation{ID: gid, Dimension: 768}, []int64{1}, 4)
	require.NoError(t, err)
	assert.Equal(t, [][]float32{v}, got[1])
}

func TestMessageVectorsExcludesNegativeChunkIndices(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)
	gid, err := b.CreateGeneration(ctx, "m", 4, "")
	require.NoError(t, err)
	chunks := []vector.Chunk{{MessageID: 1, ChunkIndex: -1, Vector: []float32{3, 4, 0, 0}}}
	want := make([][]float32, 0, 4)
	for i := range 4 {
		v := unitVec(4, i)
		chunks = append(chunks, vector.Chunk{MessageID: 1, ChunkIndex: i, Vector: v})
		want = append(want, v)
	}
	require.NoError(t, b.Upsert(ctx, gid, chunks))
	gen, err := b.BuildingGeneration(ctx)
	require.NoError(t, err)
	require.NotNil(t, gen)

	got, err := b.MessageVectors(ctx, *gen, []int64{1}, 4)
	require.NoError(t, err)
	assert.Equal(t, want, got[1])
}

func TestMessageVectorsScopesToGeneration(t *testing.T) {
	b, ctx, db := newBackendForTest(t)
	a := seedAndEmbed(t, b, db, map[int64][]float32{1: unitVec(768, 0)})
	require.NoError(t, b.ActivateGeneration(ctx, a, true))
	bg := seedAndEmbed(t, b, db, map[int64][]float32{1: unitVec(768, 1)})
	for _, tc := range []struct {
		id   vector.GenerationID
		axis int
	}{{a, 0}, {bg, 1}} {
		got, err := b.MessageVectors(ctx, vector.Generation{ID: tc.id, Dimension: 768}, []int64{1}, 4)
		require.NoError(t, err)
		assert.Equal(t, [][]float32{unitVec(768, tc.axis)}, got[1])
	}
}

func TestMessageVectorsPagesAndCaps(t *testing.T) {
	b, ctx, db := newBackendForTest(t)
	ids := make([]int64, 257)
	vecs := make(map[int64][]float32, len(ids))
	for i := range ids {
		ids[i] = int64(i + 1)
		vecs[ids[i]] = unitVec(768, i)
	}
	gid := seedAndEmbed(t, b, db, vecs)
	gen := vector.Generation{ID: gid, Dimension: 768}
	for _, n := range []int{256, 257} {
		got, err := b.MessageVectors(ctx, gen, ids[:n], 4)
		require.NoError(t, err)
		assert.Len(t, got, n)
		for i, id := range ids[:n] {
			assert.Equal(t, [][]float32{unitVec(768, i)}, got[id])
		}
	}
	// Duplicates spanning page boundaries must not append the same chunks twice.
	got, err := b.MessageVectors(ctx, gen, slices.Concat(ids, ids), 4)
	require.NoError(t, err)
	assert.Len(t, got, 257)
	assert.Equal(t, [][]float32{unitVec(768, 0)}, got[1])

	// A zero-value backend proves the cap is enforced before any database query,
	// even when deduplication would reduce the input below the cap.
	_, err = (&Backend{}).MessageVectors(ctx, gen, make([]int64, 2001), 4)
	assert.ErrorIs(t, err, vector.ErrFilterTooLarge)
}

func TestMessageVectorsRejectsInvalidArguments(t *testing.T) {
	for _, tc := range []struct {
		name           string
		dim, maxChunks int
	}{{"zero chunks", 768, 0}, {"negative chunks", 768, -1}, {"zero dimension", 0, 4}, {"negative dimension", -1, 4}} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (&Backend{}).MessageVectors(context.Background(), vector.Generation{Dimension: tc.dim}, []int64{1}, tc.maxChunks)
			assert.Error(t, err)
		})
	}
}

func TestMessageVectorsEmptyIDs(t *testing.T) {
	got, err := (&Backend{}).MessageVectors(context.Background(), vector.Generation{}, nil, 0)
	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.Empty(t, got)
}

func TestMessageVectorsCancelledContext(t *testing.T) {
	b, ctx, db := newBackendForTest(t)
	gid := seedAndEmbed(t, b, db, map[int64][]float32{1: unitVec(768, 0)})
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err := b.MessageVectors(ctx, vector.Generation{ID: gid, Dimension: 768}, []int64{1}, 4)
	assert.ErrorIs(t, err, context.Canceled)
}
