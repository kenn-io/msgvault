//go:build pgvector

package pgvector

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

func TestMessageCosinesScoresStoredChunks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	b, ctx, db := newBackendForTest(t)
	_, err := db.Exec(`INSERT INTO messages (id) VALUES (2), (3), (4), (5)`)
	require.NoError(err)
	gen, err := b.CreateGeneration(ctx, "model", 768, "")
	require.NoError(err)
	vec := func(x, y float32) []float32 {
		v := make([]float32, 768)
		v[0], v[1] = x, y
		return v
	}
	require.NoError(b.Upsert(ctx, gen, []vector.Chunk{
		{MessageID: 1, Vector: vec(3, 0)},
		{MessageID: 2, Vector: vec(-1, 0)},
		{MessageID: 3, Vector: vec(0, 0)},
		{MessageID: 4, Vector: vec(0, 2)},
		{MessageID: 4, ChunkIndex: 1, Vector: vec(2, 2)},
	}))
	require.NoError(b.ActivateGeneration(ctx, gen, true))
	other, err := b.CreateGeneration(ctx, "other-model", 768, "")
	require.NoError(err)
	require.NoError(b.Upsert(ctx, other, []vector.Chunk{{MessageID: 5, Vector: vec(1, 0)}}))

	generation := vector.Generation{ID: gen, Dimension: 768}
	ids := []int64{1, 2, 3, 4, 5, 6}
	got, err := b.MessageCosines(ctx, generation, vec(1, 0), ids)
	require.NoError(err)
	require.Len(got.Scores, 4, "missing chunks and other generations do not count")
	assert.InDelta(1, got.Scores[1], 1e-9, "vector length does not change cosine")
	assert.InDelta(-1, got.Scores[2], 1e-9)
	assert.Zero(got.Scores[3], "a stored zero vector scores zero")
	assert.InDelta(1/math.Sqrt2, got.Scores[4], 1e-6, "the best chunk scores the message")

	got, err = b.MessageCosines(ctx, generation, vec(0, 0), ids)
	require.NoError(err)
	assert.Equal(map[int64]float64{1: 0, 2: 0, 3: 0, 4: 0}, got.Scores, "a zero query scores zero")
	got, err = b.MessageCosines(ctx, generation, vec(1, 0), nil)
	require.NoError(err)
	assert.Empty(got.Scores)
}
