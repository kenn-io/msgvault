//go:build sqlite_vec

package cmd

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
	"go.kenn.io/msgvault/internal/vector/sqlitevec/vec1"
)

// installListAcceleratorRow inserts a vector_accelerators row the way the
// optimize publish path leaves it: state='ready', counts and revision
// matching the generation, and the installed Vec1 version.
func installListAcceleratorRow(
	t *testing.T,
	backend *sqlitevec.Backend,
	generationID vector.GenerationID,
	state string,
) {
	t.Helper()
	tableName := fmt.Sprintf("message_ann_g%d", int64(generationID))
	_, err := backend.DB().Exec(`CREATE VIRTUAL TABLE ` + tableName + ` USING vec1(embedding)`)
	require.NoError(t, err)
	_, err = backend.DB().Exec(`INSERT INTO `+tableName+`(rowid, embedding)
		SELECT embedding_id, embedding FROM `+sqlitevec.VectorTableName(4)+`
		WHERE generation_id = ?`, int64(generationID))
	require.NoError(t, err)
	var count, revision int64
	require.NoError(t, backend.DB().QueryRow(`SELECT embedding_count, vector_revision
		FROM index_generations WHERE id = ?`, int64(generationID)).Scan(&count, &revision))
	now := time.Now().Unix()
	_, err = backend.DB().Exec(`INSERT INTO vector_accelerators
		(generation_id, kind, state, table_name, dimension, indexed_count,
		 last_embedding_id, source_revision, started_at, completed_at,
		 model_config, vec1_version)
		VALUES (?, 'vec1_ivf_opq', ?, ?, ?, ?, 0, ?, ?, ?, '{}', ?)`,
		int64(generationID), state, tableName, 4, count, revision, now, now,
		vec1.Version)
	require.NoError(t, err)
}

// openListBackend seeds a fresh vectors.db with one generation holding two
// embedded messages and returns the backend plus the generation ID.
func openListBackend(t *testing.T) (*sqlitevec.Backend, vector.GenerationID) {
	t.Helper()
	backend, err := sqlitevec.Open(t.Context(), sqlitevec.Options{
		Path:      filepath.Join(t.TempDir(), "vectors.db"),
		Dimension: 4,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, backend.Close()) })
	generationID, err := backend.CreateGeneration(t.Context(), "model", 4, "model:test")
	require.NoError(t, err)
	require.NoError(t, backend.Upsert(t.Context(), generationID, []vector.Chunk{
		{MessageID: 10, Vector: []float32{1, 0, 0, 0}},
		{MessageID: 11, Vector: []float32{0, 1, 0, 0}},
	}))
	return backend, generationID
}

func TestReadEmbeddingAcceleratorDisplayStates(t *testing.T) {
	t.Run("absent displays exact", func(t *testing.T) {
		backend, generationID := openListBackend(t)

		row, err := readEmbeddingAccelerator(t.Context(), backend, generationID)
		require.NoError(t, err)
		assert.Equal(t, "exact", row.State)
	})

	t.Run("building displays building", func(t *testing.T) {
		backend, generationID := openListBackend(t)
		installListAcceleratorRow(t, backend, generationID, "building")

		row, err := readEmbeddingAccelerator(t.Context(), backend, generationID)
		require.NoError(t, err)
		assert.Equal(t, "building", row.State)
	})

	t.Run("eligible stored ready displays ready", func(t *testing.T) {
		backend, generationID := openListBackend(t)
		installListAcceleratorRow(t, backend, generationID, "ready")

		row, err := readEmbeddingAccelerator(t.Context(), backend, generationID)
		require.NoError(t, err)
		assert.Equal(t, "ready", row.State)
		assert.Equal(t, int64(2), row.IndexedCount)
	})

	t.Run("ineligible stored ready displays stale", func(t *testing.T) {
		backend, generationID := openListBackend(t)
		installListAcceleratorRow(t, backend, generationID, "ready")

		// A raw embeddings write fires the vector_revision maintenance
		// trigger, making the stored-ready accelerator ineligible for
		// search. The stored row still says ready; the CLI must display the
		// effective state: stale.
		_, err := backend.DB().Exec(`UPDATE embeddings
			SET message_id = message_id WHERE generation_id = ?`, int64(generationID))
		require.NoError(t, err)
		raw, err := backend.Accelerator(t.Context(), generationID)
		require.NoError(t, err)
		require.NotNil(t, raw)
		assert.Equal(t, sqlitevec.AcceleratorReady, raw.State,
			"the stored state remains ready after the trigger-driven revision bump")

		row, err := readEmbeddingAccelerator(t.Context(), backend, generationID)
		require.NoError(t, err)
		assert.Equal(t, "stale", row.State,
			"an ineligible stored-ready accelerator must display stale, not the raw ready state")
	})
}
