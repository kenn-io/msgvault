//go:build sqlite_vec

package cmd

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

// TestRunEmbeddingsRetire_ForceActive drives the CLI retire path through the
// real sqlitevec backend (cf-2: the state transition routes through
// backend.RetireGeneration). It requires the sqlite_vec build tag because
// runEmbeddingsRetire opens a sqlitevec backend, whose RegisterExtension
// returns ErrNotBuilt under a no-sqlite_vec build. The untagged pre-check
// refusal lives in TestRetireEmbeddingGenerationRefusesActiveWithoutForce_PreCheck.
func TestRunEmbeddingsRetire_ForceActive(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	dbPath := newEmbeddingMetadataTestDBFile(t)
	withEmbeddingCommandConfig(t, dbPath)

	oldYes := embeddingsRetireYes
	oldForce := embeddingsRetireForceActive
	embeddingsRetireYes = true
	embeddingsRetireForceActive = true
	t.Cleanup(func() {
		embeddingsRetireYes = oldYes
		embeddingsRetireForceActive = oldForce
	})

	cmd := embeddingsRetireCmd
	oldCtx := cmd.Context()
	cmd.SetContext(context.Background())
	t.Cleanup(func() { cmd.SetContext(oldCtx) })

	require.NoError(runEmbeddingsRetire(cmd, []string{"1"}),
		"retire active generation with --force-active")

	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(db.Close()) })
	row := mustGetEmbeddingGeneration(t.Context(), t, db, 1)
	assert.Equal(vector.GenerationRetired, row.State)
}

func TestFillFullCoverageUsesEmbeddingScopeForEmbeddedCount(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	ctx := context.Background()
	dataDir := t.TempDir()
	dbPath := newEmbeddingMetadataTestDBFileAt(t, filepath.Join(dataDir, "vectors.db"))
	seedMainDBWithScopedFullCoverageMessages(t, dataDir)
	withEmbeddingCommandConfigDataDir(t, dbPath, dataDir)
	cfg.Vector.Embed.Scope.MessageTypes = []string{"sms"}

	backend, closeBackend, err := openEmbeddingsBackend(ctx)
	require.NoError(err, "open embeddings backend")
	t.Cleanup(closeBackend)
	require.NoError(backend.Upsert(ctx, 2, []vector.Chunk{
		{MessageID: 1, Vector: []float32{1, 0, 0, 0}},
		{MessageID: 2, Vector: []float32{0, 1, 0, 0}},
	}), "upsert in-scope and out-of-scope vectors")

	row := embeddingGenerationRow{ID: 2}
	require.NoError(fillFullCoverage(ctx, backend, &row))

	assert.Equal(int64(1), row.LiveCount, "only sms is in scope")
	assert.Equal(int64(1), row.EmbeddedCount, "out-of-scope email vector is excluded")
	assert.Equal(int64(0), row.BlankCount)
	assert.Equal(int64(0), row.MissingCount)
}
