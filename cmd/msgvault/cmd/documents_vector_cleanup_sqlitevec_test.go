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
	fixture, spec := documentVectorCommandFixture(t)
	vectorPath := filepath.Join(t.TempDir(), "vectors.db")
	cfg.Vector.DBPath = vectorPath

	generation, _, err := fixture.Store.EnsureDocumentVectorGeneration(t.Context(), spec)
	require.NoError(t, err)
	token := strings.Repeat("9", 64)
	backend, err := sqlitevec.Open(t.Context(), sqlitevec.Options{
		Path: vectorPath, Dimension: spec.Dimension,
	})
	require.NoError(t, err)
	require.NoError(t, backend.DocumentBackend().PutUnpublished(t.Context(), vectordocument.GenerationID(generation.ID), spec.Dimension, []vectordocument.Embedding{{
		Token: token, Vector: []float32{1, 0, 0},
	}}))
	require.NoError(t, backend.Close())

	_, err = fixture.Store.DB().Exec(fixture.Store.Rebind(`
		INSERT INTO document_vector_publications
			(generation_id, extraction_id, extraction_profile_id, canonical_blob_hash,
			 extraction_input_key, chunk_id, chunk_key, chunk_checksum, source_sequence, token, state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'ready')`), generation.ID,
		"disabled-cleanup-extraction", spec.TargetExtractionProfileID, strings.Repeat("a", 64),
		"original", 1, "disabled-cleanup-chunk", "disabled-cleanup-checksum", 1, token)
	require.NoError(t, err)
	retired, err := fixture.Store.RetireDocumentVectorGeneration(t.Context(), generation.ID, time.Now())
	require.NoError(t, err)
	require.True(t, retired)
	cfg.Vector.Enabled = false

	result, err := runConfiguredDocumentVectorGeneration(t.Context(), fixture.Store, generation.ID, 1)

	require.NoError(t, err)
	assert.True(t, result.Purged)
	assert.True(t, result.Converged)
	_, err = fixture.Store.GetDocumentVectorGeneration(t.Context(), generation.ID)
	require.ErrorContains(t, err, "not found")

	backend, err = sqlitevec.Open(t.Context(), sqlitevec.Options{Path: vectorPath})
	require.NoError(t, err)
	t.Cleanup(func() { _ = backend.Close() })
	var remaining int
	require.NoError(t, backend.DB().QueryRow(
		`SELECT COUNT(*) FROM document_vector_embeddings WHERE token = ?`, token,
	).Scan(&remaining))
	assert.Zero(t, remaining)
}
