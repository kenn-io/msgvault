//go:build sqlite_vec

package sqlitevec

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

func TestReadyAcceleratorTracksUpsertAndDeleteAtomically(t *testing.T) {
	backend := openOptimizeBackend(t, 4)
	generationID := seedOptimizeVectors(t, backend, 3, 4)
	tableName := installReadyFlatAccelerator(t, backend, generationID, 4)

	var replacedID int64
	require.NoError(t, backend.db.QueryRow(`SELECT embedding_id FROM embeddings
		WHERE generation_id = ? AND message_id = 1`, int64(generationID)).Scan(&replacedID))
	replacement := []float32{0.25, 0.5, 0.75, 1}
	require.NoError(t, backend.Upsert(t.Context(), generationID, []vector.Chunk{{
		MessageID: 1, Vector: replacement,
	}}))

	ready, ok, err := backend.readyAccelerator(t.Context(), generationID, 4)
	require.NoError(t, err)
	require.True(t, ok, "a transactionally maintained accelerator must remain ready")
	assert.Equal(t, int64(3), ready.IndexedCount)
	var oldRows int
	require.NoError(t, backend.db.QueryRow(`SELECT COUNT(*) FROM `+tableName+` WHERE rowid = ?`, replacedID).Scan(&oldRows))
	assert.Zero(t, oldRows)
	var newID int64
	require.NoError(t, backend.db.QueryRow(`SELECT embedding_id FROM embeddings
		WHERE generation_id = ? AND message_id = 1`, int64(generationID)).Scan(&newID))
	var accelerated []byte
	require.NoError(t, backend.db.QueryRow(`SELECT embedding FROM `+tableName+` WHERE rowid = ?`, newID).Scan(&accelerated))
	assert.Equal(t, float32SliceBlob(replacement), accelerated)

	require.NoError(t, backend.Delete(t.Context(), generationID, []int64{2}))
	ready, ok, err = backend.readyAccelerator(t.Context(), generationID, 4)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(2), ready.IndexedCount)
	var acceleratorRows int64
	require.NoError(t, backend.db.QueryRow(`SELECT COUNT(*) FROM `+tableName).Scan(&acceleratorRows))
	assert.Equal(t, int64(2), acceleratorRows)
}

func TestReadyAcceleratorFailureRollsBackAuthoritativeUpsert(t *testing.T) {
	backend := openOptimizeBackend(t, 4)
	generationID := seedOptimizeVectors(t, backend, 1, 4)
	tableName := acceleratorTableName(generationID)
	_, err := backend.db.Exec(`CREATE TABLE ` + tableName + ` (
		rowid INTEGER PRIMARY KEY,
		embedding BLOB NOT NULL CHECK(length(embedding) < 4)
	)`)
	require.NoError(t, err)
	recordReadyAccelerator(t, backend, generationID, tableName, 4)

	var beforeCount, beforeRevision int64
	require.NoError(t, backend.db.QueryRow(`SELECT embedding_count, vector_revision
		FROM index_generations WHERE id = ?`, int64(generationID)).Scan(&beforeCount, &beforeRevision))
	err = backend.Upsert(t.Context(), generationID, []vector.Chunk{{
		MessageID: 2, Vector: unitVec(4, 1),
	}})
	require.Error(t, err)

	var embeddings int64
	require.NoError(t, backend.db.QueryRow(`SELECT COUNT(*) FROM embeddings
		WHERE generation_id = ?`, int64(generationID)).Scan(&embeddings))
	assert.Equal(t, int64(1), embeddings)
	var afterCount, afterRevision int64
	require.NoError(t, backend.db.QueryRow(`SELECT embedding_count, vector_revision
		FROM index_generations WHERE id = ?`, int64(generationID)).Scan(&afterCount, &afterRevision))
	assert.Equal(t, beforeCount, afterCount)
	assert.Equal(t, beforeRevision, afterRevision)
}

func TestReadyAcceleratorTracksDocumentScopePublications(t *testing.T) {
	backend := openOptimizeBackend(t, 4)
	generationID := seedOptimizeVectors(t, backend, 1, 4)
	tableName := installReadyFlatAccelerator(t, backend, generationID, 4)

	scopes := []vector.DocumentScopePublication{
		{
			ScopeKey: "scope:a", SourceSequence: 1,
			Documents: []vector.DocumentPublication{{
				Key: "doc:a", Kind: "window", Revision: "a1", SourceSequence: 1, Members: []int64{2},
			}},
			Chunks: []vector.Chunk{{MessageID: 2, Vector: unitVec(4, 1)}},
		},
		{
			ScopeKey: "scope:b", SourceSequence: 1,
			Documents: []vector.DocumentPublication{{
				Key: "doc:b", Kind: "window", Revision: "b1", SourceSequence: 1, Members: []int64{3},
			}},
			Chunks: []vector.Chunk{{MessageID: 3, Vector: unitVec(4, 2)}},
		},
	}
	require.NoError(t, backend.PublishScopes(t.Context(), generationID, scopes))
	ready, ok, err := backend.readyAccelerator(t.Context(), generationID, 4)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(3), ready.IndexedCount)

	replacement := vector.DocumentPublication{
		Key: "doc:a", Kind: "window", Revision: "a2", SourceSequence: 2, Members: []int64{2},
	}
	require.NoError(t, backend.PublishScope(t.Context(), generationID, "scope:a", 2,
		[]vector.DocumentPublication{replacement},
		[]vector.Chunk{{MessageID: 2, Vector: unitVec(4, 3)}}))
	ready, ok, err = backend.readyAccelerator(t.Context(), generationID, 4)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(3), ready.IndexedCount)
	var rows int64
	require.NoError(t, backend.db.QueryRow(`SELECT COUNT(*) FROM `+tableName).Scan(&rows))
	assert.Equal(t, int64(3), rows)
}

func TestBuildingAcceleratorIsNotMaintainedByWrites(t *testing.T) {
	backend := openOptimizeBackend(t, 4)
	generationID := seedOptimizeVectors(t, backend, 1, 4)
	tableName := installReadyFlatAccelerator(t, backend, generationID, 4)
	_, err := backend.db.Exec(`UPDATE vector_accelerators SET state = 'building' WHERE generation_id = ?`,
		int64(generationID))
	require.NoError(t, err)

	require.NoError(t, backend.Upsert(t.Context(), generationID, []vector.Chunk{{
		MessageID: 2, Vector: unitVec(4, 1),
	}}))
	var rows int64
	require.NoError(t, backend.db.QueryRow(`SELECT COUNT(*) FROM `+tableName).Scan(&rows))
	assert.Equal(t, int64(1), rows)
	_, ready, err := backend.readyAccelerator(t.Context(), generationID, 4)
	require.NoError(t, err)
	assert.False(t, ready)
}

func installReadyFlatAccelerator(
	t *testing.T,
	backend *Backend,
	generationID vector.GenerationID,
	dimension int,
) string {
	t.Helper()
	tableName := acceleratorTableName(generationID)
	_, err := backend.db.Exec(`CREATE VIRTUAL TABLE ` + tableName + ` USING vec1(embedding)`)
	require.NoError(t, err)
	_, err = backend.db.Exec(`INSERT INTO `+tableName+`(rowid, embedding)
		SELECT embedding_id, embedding FROM `+VectorTableName(dimension)+`
		WHERE generation_id = ?`, int64(generationID))
	require.NoError(t, err)
	recordReadyAccelerator(t, backend, generationID, tableName, dimension)
	return tableName
}

func recordReadyAccelerator(
	t *testing.T,
	backend *Backend,
	generationID vector.GenerationID,
	tableName string,
	dimension int,
) {
	t.Helper()
	var count, revision, lastID int64
	require.NoError(t, backend.db.QueryRow(`SELECT embedding_count, vector_revision
		FROM index_generations WHERE id = ?`, int64(generationID)).Scan(&count, &revision))
	require.NoError(t, backend.db.QueryRow(`SELECT COALESCE(MAX(embedding_id), 0) FROM embeddings
		WHERE generation_id = ?`, int64(generationID)).Scan(&lastID))
	_, err := backend.db.Exec(`INSERT INTO vector_accelerators
		(generation_id, kind, state, table_name, dimension, indexed_count,
		 last_embedding_id, source_revision, started_at, completed_at, model_config, vec1_version)
		VALUES (?, ?, 'ready', ?, ?, ?, ?, ?, ?, ?, '{}', ?)`,
		int64(generationID), acceleratorKind, tableName, dimension, count, lastID,
		revision, time.Now().Unix(), time.Now().Unix(), backend.vec1Version)
	require.NoError(t, err)
}
