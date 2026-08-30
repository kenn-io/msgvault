//go:build pgvector

package pgvector

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

type orphanEmbeddingPruner interface {
	PruneOrphanEmbeddings(ctx context.Context) (int64, error)
}

func TestBackend_PruneOrphanEmbeddingsRemovesOnlyHardDeletedMessages(t *testing.T) {
	backend, _, db := newBackendForTest(t)
	_, err := db.Exec(`INSERT INTO messages (id) VALUES (2), (3)`)
	require.NoError(t, err)

	active, err := backend.CreateGeneration(t.Context(), "active-model", 4, "active:4")
	require.NoError(t, err)
	require.NoError(t, backend.Upsert(t.Context(), active, []vector.Chunk{
		{MessageID: 1, ChunkIndex: 0, Vector: unitVec(4, 0)},
		{MessageID: 1, ChunkIndex: 1, Vector: unitVec(4, 1)},
		{MessageID: 3, ChunkIndex: 0, Vector: unitVec(4, 2)},
		{MessageID: 3, ChunkIndex: 1, Vector: unitVec(4, 3)},
	}))
	require.NoError(t, backend.ActivateGeneration(t.Context(), active, true))

	building, err := backend.CreateGeneration(t.Context(), "building-model", 5, "building:5")
	require.NoError(t, err)
	require.NoError(t, backend.Upsert(t.Context(), building, []vector.Chunk{
		{MessageID: 2, ChunkIndex: 0, Vector: unitVec(5, 0)},
		{MessageID: 3, ChunkIndex: 0, Vector: unitVec(5, 1)},
	}))

	_, err = db.Exec(`
		UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = 2;
		DELETE FROM messages WHERE id = 3;`)
	require.NoError(t, err)

	pruner, ok := any(backend).(orphanEmbeddingPruner)
	require.True(t, ok, "PostgreSQL vector backend must support orphan pruning")
	pruned, err := pruner.PruneOrphanEmbeddings(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(2), pruned, "generation/message pairs pruned")

	var embeddings int64
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM embeddings`).Scan(&embeddings))
	assert.Equal(t, int64(3), embeddings, "live and soft-deleted chunks remain")

	for _, generation := range []vector.GenerationID{active, building} {
		var messageCount int64
		require.NoError(t, db.QueryRow(
			`SELECT message_count FROM index_generations WHERE id = $1`, generation,
		).Scan(&messageCount))
		assert.Equal(t, int64(1), messageCount, "generation %d message_count", generation)
	}

	pruned, err = pruner.PruneOrphanEmbeddings(t.Context())
	require.NoError(t, err)
	assert.Zero(t, pruned, "second prune is idempotent")
}

func TestBackend_PruneOrphanEmbeddingsDisablesStatementTimeout(t *testing.T) {
	backend, tracer, ctx := newTracedBackendForTest(t)
	generation := seedAndEmbed(t, backend, backend.db, map[int64][]float32{
		1: unitVec(768, 0),
	})
	require.Equal(t, 1, countEmbeddingRows(t, backend, generation))
	_, err := backend.db.ExecContext(ctx, "DELETE FROM messages WHERE id = 1")
	require.NoError(t, err)
	backend.db.SetMaxOpenConns(1)
	_, err = backend.db.ExecContext(ctx, "SET statement_timeout = 1")
	require.NoError(t, err)

	tracer.reset()
	pruned, err := backend.PruneOrphanEmbeddings(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), pruned)

	_, err = backend.db.ExecContext(ctx, "RESET statement_timeout")
	require.NoError(t, err)
	assert.True(t, tracer.contains("SET LOCAL statement_timeout = 0"),
		"prune transaction must disable the pool-wide statement timeout; got %v", tracer.snapshot())
}
