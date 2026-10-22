//go:build pgvector

package pgvector

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateDropsAttachmentDocumentVectors(t *testing.T) {
	db := openPGTestDB(t)
	_, err := db.Exec(`CREATE TABLE document_vector_backend_generations (generation_id BIGINT PRIMARY KEY);
 CREATE TABLE document_vector_embeddings (token TEXT PRIMARY KEY, generation_id BIGINT REFERENCES document_vector_backend_generations(generation_id));
 INSERT INTO document_vector_backend_generations VALUES (1);
 INSERT INTO document_vector_embeddings VALUES ('synthetic-token',1)`)
	require.NoError(t, err)
	require.NoError(t, Migrate(t.Context(), db, 4, false))
	for _, table := range []string{"document_vector_embeddings", "document_vector_backend_generations"} {
		var exists bool
		require.NoError(t, db.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists))
		assert.False(t, exists)
	}
	require.NoError(t, Migrate(t.Context(), db, 4, false))
}
