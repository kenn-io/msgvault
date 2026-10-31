//go:build pgvector

package pgvector

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

func TestXRemovalEnumeratesAndDeletesEveryGeneration(t *testing.T) {
	b, ctx, db := newBackendForTest(t)
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
	// Retirement normally deletes PG vectors. Seed one legacy retired row to
	// prove erasure performs real Delete work for that generation too.
	_, err := db.ExecContext(ctx, `INSERT INTO embeddings(generation_id,message_id,chunk_index,embedded_at,source_char_len,chunk_char_start,chunk_char_end,truncated,dimension,embedding,source_basis) VALUES($1,1,0,1,0,0,0,FALSE,768,$2::vector,0)`, int64(want[0]), vectorLiteral(embedding))
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE index_generations SET message_count=1 WHERE id=$1`, int64(want[0]))
	require.NoError(t, err)
	all, err := b.GenerationIDs(ctx)
	require.NoError(t, err)
	assert.Equal(t, want, all)
	for _, gen := range all {
		require.NoError(t, b.Delete(ctx, gen, []int64{1}))
		var n int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM embeddings WHERE generation_id=$1 AND message_id=1`, int64(gen)).Scan(&n))
		assert.Zero(t, n)
	}
}
