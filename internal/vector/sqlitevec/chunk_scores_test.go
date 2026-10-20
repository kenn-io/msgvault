//go:build sqlite_vec

package sqlitevec

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Chunk scores are 1 - cosine distance, as in pgvector, even for vectors that
// are not unit length. A 1 - L2 score would give 0 and 0.17 here.
func TestScoreMessageChunksIsCosineSimilarity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	b, ctx := newFusedBackendForTest(t)
	long := make([]float32, 768)
	long[0] = 3
	diagonal := make([]float32, 768)
	diagonal[0], diagonal[1] = 1, 1
	gen := seedAndEmbed(t, b, map[int64][]float32{1: long, 2: diagonal, 3: unitVec(768, 2)})
	require.NoError(b.ActivateGeneration(ctx, gen, true))

	for id, want := range map[int64]float64{1: 1, 2: 1 / math.Sqrt2, 3: 0} {
		chunks, err := b.ScoreMessageChunks(ctx, gen, id, unitVec(768, 0))
		require.NoError(err)
		require.Len(chunks, 1)
		assert.InDelta(want, chunks[0].Score, 1e-6, "message %d", id)
	}
}
