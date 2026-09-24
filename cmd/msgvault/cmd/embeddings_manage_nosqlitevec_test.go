//go:build !sqlite_vec

package cmd

import (
	"bytes"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

func TestRunEmbeddingsListRetiredOnlySucceedsWithoutSQLiteVec(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	vectorPath := filepath.Join(dir, "vectors.db")
	c := config.NewDefaultConfig()
	c.HomeDir = dir
	c.Data.DataDir = dir
	c.Vector.Enabled = true
	c.Vector.DBPath = vectorPath
	c.Vector.Embeddings.Model = "test-model"
	c.Vector.Embeddings.Dimension = 4
	withTestConfig(t, c)

	mainStore, err := store.Open(c.DatabaseDSN())
	require.NoError(err)
	require.NoError(mainStore.InitSchema())
	require.NoError(mainStore.Close())

	vectorsDB, err := sql.Open("sqlite3", vectorPath)
	require.NoError(err)
	_, err = vectorsDB.Exec(`CREATE TABLE index_generations (
		id INTEGER PRIMARY KEY,
		model TEXT NOT NULL,
		dimension INTEGER NOT NULL,
		fingerprint TEXT NOT NULL,
		state TEXT NOT NULL,
		started_at INTEGER NOT NULL,
		completed_at INTEGER,
		activated_at INTEGER,
		message_count INTEGER NOT NULL DEFAULT 0,
		seeded_at INTEGER
	)`)
	require.NoError(err)
	_, err = vectorsDB.Exec(`INSERT INTO index_generations
		(id, model, dimension, fingerprint, state, started_at, message_count)
		VALUES (1, 'test-model', 4, 'test:4', ?, 1700000000, 0)`,
		string(vector.GenerationRetired))
	require.NoError(err)
	require.NoError(vectorsDB.Close())

	var output bytes.Buffer
	command := &cobra.Command{Use: "list"}
	command.SetContext(t.Context())
	command.SetOut(&output)
	require.NoError(runEmbeddingsList(command, nil),
		"retired-only listing must not require the sqlite_vec backend")
	assert.Contains(output.String(), "test-model")
	assert.Contains(output.String(), string(vector.GenerationRetired))
	assert.Contains(output.String(), "test:4")
}
