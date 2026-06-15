package cmd

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

func TestEmbeddingsCommandRegistration(t *testing.T) {
	require := requirepkg.New(t)

	buildCmd, _, err := rootCmd.Find([]string{"embeddings", "build"})
	require.NoError(err)
	require.Equal("build", buildCmd.Name())
	require.NotNil(buildCmd.Flags().Lookup("full-rebuild"))
	require.NotNil(buildCmd.Flags().Lookup("yes"))

	resumeCmd, _, err := rootCmd.Find([]string{"embeddings", "resume"})
	require.NoError(err)
	require.Equal("resume", resumeCmd.Name())
	require.Nil(resumeCmd.Flags().Lookup("full-rebuild"))

	listCmd, _, err := rootCmd.Find([]string{"embeddings", "list"})
	require.NoError(err)
	require.Equal("list", listCmd.Name())

	retireCmd, _, err := rootCmd.Find([]string{"embeddings", "retire"})
	require.NoError(err)
	require.Equal("retire", retireCmd.Name())
	require.NotNil(retireCmd.Flags().Lookup("yes"))
	require.NotNil(retireCmd.Flags().Lookup("force-active"))

	activateCmd, _, err := rootCmd.Find([]string{"embeddings", "activate"})
	require.NoError(err)
	require.Equal("activate", activateCmd.Name())
	require.NotNil(activateCmd.Flags().Lookup("yes"))
	require.NotNil(activateCmd.Flags().Lookup("force"))

	legacyCmd, _, err := rootCmd.Find([]string{"build-embeddings"})
	require.NoError(err)
	require.Equal("build-embeddings", legacyCmd.Name())
	require.NotEmpty(legacyCmd.Deprecated)
	require.NotNil(legacyCmd.Flags().Lookup("full-rebuild"))
	require.NotNil(legacyCmd.Flags().Lookup("yes"))
}

func TestListEmbeddingGenerationsIncludesActiveAndBuilding(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	db := newEmbeddingMetadataTestDB(t)

	rows, err := listEmbeddingGenerations(t.Context(), db, sqliteRebind)
	require.NoError(err)
	require.Len(rows, 2)

	assert.Equal(vector.GenerationID(1), rows[0].ID)
	assert.Equal(vector.GenerationActive, rows[0].State)
	assert.Equal(int64(2), rows[0].MessageCount)
	assert.Equal(int64(0), rows[0].PendingCount)

	assert.Equal(vector.GenerationID(2), rows[1].ID)
	assert.Equal(vector.GenerationBuilding, rows[1].State)
	assert.Equal(int64(1), rows[1].PendingCount)
}

func TestRunEmbeddingsActivateRefusesPendingWithoutForce(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	dbPath := newEmbeddingMetadataTestDBFile(t)
	withEmbeddingCommandConfig(t, dbPath)

	oldYes := embeddingsActivateYes
	embeddingsActivateYes = true
	t.Cleanup(func() { embeddingsActivateYes = oldYes })
	cmd := embeddingsActivateCmd
	oldCtx := cmd.Context()
	cmd.SetContext(context.Background())
	t.Cleanup(func() { cmd.SetContext(oldCtx) })
	err := runEmbeddingsActivate(cmd, []string{"2"})

	require.Error(err)
	assert.Contains(err.Error(), "pending embedding rows")
}

// TestRetireEmbeddingGenerationRefusesActiveWithoutForce_PreCheck pins the
// CLI UX gate that runs against the committed metadata read BEFORE any
// backend connection: retiring an active generation without --force-active
// must fail fast. The positive (force-active) path drives a real backend
// transition and lives in the sqlite_vec-tagged
// TestRunEmbeddingsRetire_ForceActive so this untagged test stays buildable
// without a vector backend tag.
func TestRetireEmbeddingGenerationRefusesActiveWithoutForce_PreCheck(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	dbPath := newEmbeddingMetadataTestDBFile(t)
	withEmbeddingCommandConfig(t, dbPath)

	oldYes := embeddingsRetireYes
	oldForce := embeddingsRetireForceActive
	embeddingsRetireYes = true
	embeddingsRetireForceActive = false
	t.Cleanup(func() {
		embeddingsRetireYes = oldYes
		embeddingsRetireForceActive = oldForce
	})

	cmd := embeddingsRetireCmd
	oldCtx := cmd.Context()
	cmd.SetContext(context.Background())
	t.Cleanup(func() { cmd.SetContext(oldCtx) })

	err := runEmbeddingsRetire(cmd, []string{"1"})
	require.Error(err)
	assert.Contains(err.Error(), "active")
}

func newEmbeddingMetadataTestDB(t *testing.T) *sql.DB {
	t.Helper()
	path := newEmbeddingMetadataTestDBFile(t)

	db, err := sql.Open("sqlite3", path)
	requirepkg.NoError(t, err)
	t.Cleanup(func() { requirepkg.NoError(t, db.Close()) })
	return db
}

func newEmbeddingMetadataTestDBFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vectors.db")
	db, err := sql.Open("sqlite3", path)
	requirepkg.NoError(t, err)
	defer func() { requirepkg.NoError(t, db.Close()) }()

	_, err = db.Exec(`
CREATE TABLE index_generations (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	model TEXT NOT NULL,
	dimension INTEGER NOT NULL,
	fingerprint TEXT NOT NULL,
	started_at INTEGER NOT NULL,
	seeded_at INTEGER,
	completed_at INTEGER,
	activated_at INTEGER,
	state TEXT NOT NULL,
	message_count INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE pending_embeddings (
	generation_id INTEGER NOT NULL,
	message_id INTEGER NOT NULL,
	enqueued_at INTEGER NOT NULL,
	claimed_at INTEGER,
	claim_token TEXT,
	PRIMARY KEY (generation_id, message_id)
);
`)
	requirepkg.NoError(t, err)

	fp := newTestConfigForFingerprint("").Vector.GenerationFingerprint()
	_, err = db.Exec(`
INSERT INTO index_generations
	(id, model, dimension, fingerprint, started_at, seeded_at, completed_at, activated_at, state, message_count)
VALUES
	(1, 'model', 4, ?, 100, 101, 110, 111, 'active', 2),
	(2, 'model', 4, ?, 120, 121, NULL, NULL, 'building', 1);
INSERT INTO pending_embeddings (generation_id, message_id, enqueued_at) VALUES (2, 42, 120);
`, fp, fp)
	requirepkg.NoError(t, err)
	return path
}

func withEmbeddingCommandConfig(t *testing.T, vecPath string) {
	t.Helper()
	oldCfg := cfg
	cfg = newTestConfigForFingerprint(vecPath)
	t.Cleanup(func() { cfg = oldCfg })
}

func newTestConfigForFingerprint(vecPath string) *config.Config {
	return &config.Config{
		Vector: vector.Config{
			Enabled: true,
			DBPath:  vecPath,
			Embeddings: vector.EmbeddingsConfig{
				Model:         "model",
				Dimension:     4,
				MaxInputChars: 32768,
			},
		},
	}
}

// sqliteRebind is the identity rebind function used by tests that operate
// directly against SQLite. It mirrors (&store.SQLiteDialect{}).Rebind.
var sqliteRebind = (&store.SQLiteDialect{}).Rebind

func mustGetEmbeddingGeneration(ctx context.Context, t *testing.T, db *sql.DB, gen vector.GenerationID) embeddingGenerationRow {
	t.Helper()
	row, err := getEmbeddingGeneration(ctx, db, sqliteRebind, gen)
	requirepkg.NoError(t, err)
	return row
}
