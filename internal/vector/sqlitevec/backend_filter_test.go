//go:build sqlite_vec

package sqlitevec

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

func TestVectorFilterMessageIDsBeforeRanking(t *testing.T) {
	b, ctx := newFusedBackendForTest(t)
	gen := seedAndEmbed(t, b, map[int64][]float32{
		1: unitVec(768, 0), 2: unitVec(768, 0), 3: unitVec(768, 1),
	})

	hits, err := b.Search(ctx, gen, unitVec(768, 0), 10, vector.Filter{MessageIDs: []int64{2}})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, int64(2), hits[0].MessageID)

	fused, _, err := b.FusedSearch(ctx, vector.FusedRequest{
		FTSTerms: []string{"meeting"}, QueryVec: unitVec(768, 0), Generation: gen,
		KPerSignal: 10, Limit: 10, RRFK: 60, Filter: vector.Filter{MessageIDs: []int64{2}},
	})
	require.NoError(t, err)
	require.Len(t, fused, 1)
	assert.Equal(t, int64(2), fused[0].MessageID)

	tooMany := make([]int64, vector.MaxFilterMessageIDs+1)
	_, err = b.Search(ctx, gen, unitVec(768, 0), 10, vector.Filter{MessageIDs: tooMany})
	require.ErrorIs(t, err, vector.ErrFilterTooLarge)
	_, _, err = b.FusedSearch(ctx, vector.FusedRequest{
		FTSTerms: []string{"meeting"}, Generation: gen, KPerSignal: 10, Limit: 10,
		Filter: vector.Filter{MessageIDs: tooMany},
	})
	require.ErrorIs(t, err, vector.ErrFilterTooLarge)
}
