//go:build sqlite_vec

package cmd

import (
	"context"
	"database/sql"
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
