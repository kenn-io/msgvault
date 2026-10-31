//go:build sqlite_vec

package sqlitevec

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

func TestXRemovalEnumeratesAndDeletesEveryGeneration(t *testing.T) {
	b, ctx := newBackendForTest(t)
	embedding := make([]float32, 768)
	embedding[0] = 1
	var want []vector.GenerationID
	for i := 0; i < 3; i++ {
		gen, err := b.CreateGeneration(ctx, "synthetic-local", 768, "")
		require.NoError(t, err)
		want = append(want, gen)
		require.NoError(t, b.Upsert(ctx, gen, []vector.Chunk{{MessageID: 1, Vector: embedding}}))
		if i < 2 {
			require.NoError(t, b.ActivateGeneration(ctx, gen, true))
		}
	}
	all, err := b.GenerationIDs(ctx)
	require.NoError(t, err)
	assert.Equal(t, want, all)
	for _, gen := range all {
		require.NoError(t, b.Delete(ctx, gen, []int64{1}))
		var n int
		require.NoError(t, b.DB().QueryRow(`SELECT count(*) FROM embeddings WHERE generation_id=? AND message_id=1`, int64(gen)).Scan(&n))
		assert.Zero(t, n)
	}
}
