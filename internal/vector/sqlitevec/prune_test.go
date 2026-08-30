//go:build sqlite_vec

package sqlitevec

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

func TestBackend_PruneOrphanEmbeddingsRemovesOnlyHardDeletedMessages(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.db")
	mainDB, err := sql.Open("sqlite3", mainPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mainDB.Close() })
	_, err = mainDB.Exec(`
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY,
			deleted_at DATETIME,
			deleted_from_source_at DATETIME,
			embed_gen INTEGER
		);
		INSERT INTO messages (id) VALUES (1), (2), (3);`)
	require.NoError(t, err)

	backend, err := Open(t.Context(), Options{
		Path: filepath.Join(dir, "vectors.db"), MainPath: mainPath,
		Dimension: 4, MainDB: mainDB,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = backend.Close() })

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

	_, err = mainDB.Exec(`
		UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = 2;
		DELETE FROM messages WHERE id = 3;`)
	require.NoError(t, err)

	pruner, ok := any(backend).(vector.OrphanEmbeddingPruner)
	require.True(t, ok, "sqlite vector backend must support orphan pruning")
	pruned, err := pruner.PruneOrphanEmbeddings(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(2), pruned, "generation/message pairs pruned")

	var embeddings, vectors4, vectors5 int64
	require.NoError(t, backend.db.QueryRow(`SELECT COUNT(*) FROM embeddings`).Scan(&embeddings))
	require.NoError(t, backend.db.QueryRow(`SELECT COUNT(*) FROM vectors_vec_d4`).Scan(&vectors4))
	require.NoError(t, backend.db.QueryRow(`SELECT COUNT(*) FROM vectors_vec_d5`).Scan(&vectors5))
	assert.Equal(t, int64(3), embeddings, "live and soft-deleted chunks remain")
	assert.Equal(t, int64(2), vectors4, "active generation vec0 rows")
	assert.Equal(t, int64(1), vectors5, "building generation vec0 rows")
	assert.Equal(t, embeddings, vectors4+vectors5, "metadata and vec0 rows stay aligned")

	for _, generation := range []vector.GenerationID{active, building} {
		var messageCount int64
		require.NoError(t, backend.db.QueryRow(
			`SELECT message_count FROM index_generations WHERE id = ?`, generation,
		).Scan(&messageCount))
		assert.Equal(t, int64(1), messageCount, "generation %d message_count", generation)
	}

	pruned, err = pruner.PruneOrphanEmbeddings(t.Context())
	require.NoError(t, err)
	assert.Zero(t, pruned, "second prune is idempotent")
}
