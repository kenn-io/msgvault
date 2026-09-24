//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

func TestReadyAcceleratorFailsClosedAfterLegacyWrite(t *testing.T) {
	ctx := context.Background()
	backend, err := Open(ctx, Options{
		Path:      filepath.Join(t.TempDir(), "vectors.db"),
		Dimension: 4,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, backend.Close()) })

	generationID, err := backend.CreateGeneration(ctx, "model", 4, "model:4")
	require.NoError(t, err)
	require.NoError(t, backend.Upsert(ctx, generationID, []vector.Chunk{
		{MessageID: 10, Vector: unitVec(4, 0)},
	}))

	tableName := acceleratorTableName(generationID)
	_, err = backend.db.Exec(`CREATE VIRTUAL TABLE ` + tableName + ` USING vec1(embedding)`)
	require.NoError(t, err)
	_, err = backend.db.Exec(`INSERT INTO `+tableName+`(rowid, embedding)
		SELECT v.embedding_id, v.embedding
		FROM vectors_vec_d4 v WHERE v.generation_id = ?`, int64(generationID))
	require.NoError(t, err)
	_, err = backend.db.Exec(`INSERT INTO vector_accelerators
		(generation_id, kind, state, table_name, dimension, indexed_count,
		 source_revision, started_at, completed_at, model_config, vec1_version)
		SELECT id, 'vec1_ivf_opq', 'ready', ?, dimension, embedding_count,
		       vector_revision, 1, 1, '{}', '0.7'
		FROM index_generations WHERE id = ?`, tableName, int64(generationID))
	require.NoError(t, err)

	accelerator, ok, err := backend.readyAccelerator(ctx, generationID, 4)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, tableName, accelerator.TableName)

	// This is the shape of a write from an older binary: it knows only the
	// authoritative embeddings/sqlite-vec tables, not vector_accelerators.
	_, err = backend.db.Exec(`DELETE FROM vectors_vec_d4 WHERE generation_id = ?`, int64(generationID))
	require.NoError(t, err)
	_, err = backend.db.Exec(`DELETE FROM embeddings WHERE generation_id = ?`, int64(generationID))
	require.NoError(t, err)

	_, ok, err = backend.readyAccelerator(ctx, generationID, 4)
	require.NoError(t, err)
	assert.False(t, ok)
}
