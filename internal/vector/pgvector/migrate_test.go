//go:build pgvector

package pgvector

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrate_ReclaimsRetiredDocumentVectors(t *testing.T) {
	db := openPGTestDB(t)
	_, err := db.Exec(`CREATE EXTENSION IF NOT EXISTS vector`)
	require.NoError(t, err)
	_, err = db.Exec(schemaSQL)
	require.NoError(t, err)
	retiredSchema, err := os.ReadFile("testdata/retired_document_vectors.sql")
	require.NoError(t, err)
	_, err = db.Exec(string(retiredSchema))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO document_vector_backend_generations (generation_id, dimension) VALUES (1, 4);
		INSERT INTO document_vector_embeddings (token, generation_id, dimension, embedding)
		VALUES ('retired', 1, 4, '[1,0,0,0]')`)
	require.NoError(t, err)

	for range 2 {
		require.NoError(t, Migrate(t.Context(), db, 4, false))
		var remaining int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM pg_class
			WHERE relnamespace = current_schema()::regnamespace
			AND relname IN ('document_vector_embeddings', 'document_vector_backend_generations', 'idx_document_vector_embeddings_generation')`).Scan(&remaining))
		assert.Zero(t, remaining, "retired vectors, generations, and their index are reclaimed")
	}
}
