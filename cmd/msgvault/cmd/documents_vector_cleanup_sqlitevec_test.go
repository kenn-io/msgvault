//go:build sqlite_vec

package cmd

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	vectordocument "go.kenn.io/msgvault/internal/vector/document"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

func TestRunConfiguredDocumentVectorGenerationCleansRetiredWhenEmbeddingsDisabled(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture, spec := documentVectorCommandFixture(t)
	vectorPath := filepath.Join(t.TempDir(), "vectors.db")
	cfg.Vector.DBPath = vectorPath

	generation, _, err := fixture.Store.EnsureDocumentVectorGeneration(t.Context(), spec)
	require.NoError(err)
	token := strings.Repeat("9", 64)
	backend, err := sqlitevec.Open(t.Context(), sqlitevec.Options{
		Path: vectorPath, Dimension: spec.Dimension,
	})
	require.NoError(err)
	require.NoError(backend.DocumentBackend().PutUnpublished(t.Context(), vectordocument.GenerationID(generation.ID), spec.Dimension, []vectordocument.Embedding{{
		Token: token, Vector: []float32{1, 0, 0},
	}}))
	require.NoError(backend.Close())

	_, err = fixture.Store.DB().Exec(fixture.Store.Rebind(`
		INSERT INTO document_vector_publications
			(generation_id, extraction_id, extraction_profile_id, canonical_blob_hash,
			 extraction_input_key, chunk_id, chunk_key, chunk_checksum, source_sequence, token, state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'ready')`), generation.ID,
		"disabled-cleanup-extraction", spec.TargetExtractionProfileID, strings.Repeat("a", 64),
		"original", 1, "disabled-cleanup-chunk", "disabled-cleanup-checksum", 1, token)
	require.NoError(err)
	retired, err := fixture.Store.RetireDocumentVectorGeneration(t.Context(), generation.ID, time.Now())
	require.NoError(err)
	require.True(retired)
	cfg.Vector.Enabled = false

	result, err := runConfiguredDocumentVectorGeneration(t.Context(), fixture.Store, generation.ID, 1)

	require.NoError(err)
	assert.True(result.Purged)
	assert.True(result.Converged)
	_, err = fixture.Store.GetDocumentVectorGeneration(t.Context(), generation.ID)
	require.ErrorContains(err, "not found")

	backend, err = sqlitevec.Open(t.Context(), sqlitevec.Options{Path: vectorPath})
	require.NoError(err)
	t.Cleanup(func() { _ = backend.Close() })
	var remaining int
	require.NoError(backend.DB().QueryRow(
		`SELECT COUNT(*) FROM document_vector_embeddings WHERE token = ?`, token,
	).Scan(&remaining))
	assert.Zero(remaining)
}
