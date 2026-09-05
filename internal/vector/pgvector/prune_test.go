//go:build pgvector

package pgvector

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

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

	pruner, ok := any(backend).(vector.OrphanEmbeddingPruner)
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
	backend, ctx, db := newBackendForTest(t)
	generation := seedAndEmbed(t, backend, backend.db, map[int64][]float32{
		1: unitVec(768, 0),
	})
	require.Equal(t, 1, countEmbeddingRows(t, backend, generation))
	_, err := backend.db.ExecContext(ctx, "DELETE FROM messages WHERE id = 1")
	require.NoError(t, err)
	// Delay the actual DELETE beyond the session timeout. A 1 ms timeout
	// can cancel BEGIN before prune reaches SET LOCAL, and a fast DELETE
	// would not prove that the maintenance transaction lifts the timeout.
	_, err = db.ExecContext(ctx, `
		CREATE FUNCTION delay_orphan_prune() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
			PERFORM pg_sleep(1.5);
			RETURN NULL;
		END
		$$;
		CREATE TRIGGER delay_orphan_prune
		BEFORE DELETE ON embeddings
		FOR EACH STATEMENT EXECUTE FUNCTION delay_orphan_prune()`)
	require.NoError(t, err)

	low := openLowTimeoutHandle(t, db)
	lowBackend := &Backend{db: low}
	pruned, err := lowBackend.PruneOrphanEmbeddings(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), pruned)
	assert.Zero(t, countEmbeddingRows(t, backend, generation), "orphan deletion committed")
}
