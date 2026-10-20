//go:build sqlite_vec

package sqlitevec

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/vector"
)

// Scores follow pgvector's 1 - cosine distance on every search path. The
// vectors are not unit length, so a 1 - L2 score would differ.
func TestSearchScoresAreCosineSimilarity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	b, ctx := newFusedBackendForTest(t)
	same := unitVec(768, 0)
	scaled := make([]float32, 768)
	scaled[0] = 3
	diagonal := unitVec(768, 0)
	diagonal[1] = 1
	gen := seedAndEmbed(t, b, map[int64][]float32{1: scaled, 2: diagonal, 3: unitVec(768, 2)})
	require.NoError(b.ActivateGeneration(ctx, gen, true))
	want := map[int64]float64{1: 1, 2: 1 / math.Sqrt2, 3: 0}

	check := func(name string, hits []vector.Hit) {
		require.Len(hits, 3, name)
		for _, h := range hits {
			assert.InDelta(want[h.MessageID], h.Score, 1e-6, "%s message %d", name, h.MessageID)
		}
	}
	hits, err := b.Search(ctx, gen, same, 3, vector.Filter{})
	require.NoError(err)
	check("unfiltered", hits)
	hits, err = b.Search(ctx, gen, same, 3, vector.Filter{MessageIDs: []int64{1, 2, 3}})
	require.NoError(err)
	check("filtered", hits)

	fused, _, err := b.FusedSearch(ctx, vector.FusedRequest{
		QueryVec: same, Generation: gen, KPerSignal: 10, Limit: 3, RRFK: 60,
	})
	require.NoError(err)
	require.Len(fused, 3)
	for _, h := range fused {
		assert.InDelta(want[h.MessageID], h.VectorScore, 1e-6, "fused message %d", h.MessageID)
	}

	for id, score := range want {
		chunks, err := b.ScoreMessageChunks(ctx, gen, id, same)
		require.NoError(err)
		require.Len(chunks, 1)
		assert.InDelta(score, chunks[0].Score, 1e-6, "chunk of message %d", id)
	}
}
