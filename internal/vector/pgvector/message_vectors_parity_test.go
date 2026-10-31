//go:build sqlite_vec && pgvector

package pgvector

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/vector"
)

func TestMessageVectorsParity(t *testing.T) {
	corpus := parityCorpus[:3]
	pf, pgGen := buildPgvectorParity(t, corpus)
	sb, sctx, sGen := buildSqlitevecParity(t, corpus)

	chunks := []vector.Chunk{{MessageID: 3, Vector: []float32{-0.25, 0.5, 1, 0}}}
	for i := 5; i >= 0; i-- {
		chunks = append(chunks,
			vector.Chunk{MessageID: 1, ChunkIndex: i, Vector: []float32{0.5, -0.25, 1, float32(i)}},
			vector.Chunk{MessageID: 2, ChunkIndex: i, Vector: []float32{1, 0.5, -0.25, float32(i)}},
		)
	}
	require.NoError(t, pf.b.Upsert(pf.ctx, pgGen, chunks))
	require.NoError(t, sb.Upsert(sctx, sGen, chunks))

	for _, maxChunks := range []int{1, 4} {
		sqliteResult, err := sb.MessageVectors(sctx, vector.Generation{ID: sGen, Dimension: parityDim}, []int64{3, 1, 2}, maxChunks)
		require.NoError(t, err)
		pgResult, err := pf.b.MessageVectors(pf.ctx, vector.Generation{ID: pgGen, Dimension: parityDim}, []int64{3, 1, 2}, maxChunks)
		require.NoError(t, err)
		require.Len(t, sqliteResult, 3)
		require.Len(t, sqliteResult[1], maxChunks)
		assert.Equal(t, sqliteResult, pgResult)
	}
}
