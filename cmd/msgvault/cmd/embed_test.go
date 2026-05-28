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
	"go.kenn.io/msgvault/internal/vector"
)

func TestEmbeddingsCommandRegistration(t *testing.T) {
	buildCmd, _, err := rootCmd.Find([]string{"embeddings", "build"})
	requirepkg.NoError(t, err)
	requirepkg.Equal(t, "build", buildCmd.Name())
	requirepkg.NotNil(t, buildCmd.Flags().Lookup("full-rebuild"))
	requirepkg.NotNil(t, buildCmd.Flags().Lookup("yes"))

	listCmd, _, err := rootCmd.Find([]string{"embeddings", "list"})
	requirepkg.NoError(t, err)
	requirepkg.Equal(t, "list", listCmd.Name())

	retireCmd, _, err := rootCmd.Find([]string{"embeddings", "retire"})
	requirepkg.NoError(t, err)
	requirepkg.Equal(t, "retire", retireCmd.Name())
	requirepkg.NotNil(t, retireCmd.Flags().Lookup("yes"))
	requirepkg.NotNil(t, retireCmd.Flags().Lookup("force-active"))

	activateCmd, _, err := rootCmd.Find([]string{"embeddings", "activate"})
	requirepkg.NoError(t, err)
	requirepkg.Equal(t, "activate", activateCmd.Name())
	requirepkg.NotNil(t, activateCmd.Flags().Lookup("yes"))
	requirepkg.NotNil(t, activateCmd.Flags().Lookup("force"))

	legacyCmd, _, err := rootCmd.Find([]string{"build-embeddings"})
	requirepkg.NoError(t, err)
	requirepkg.Equal(t, "build-embeddings", legacyCmd.Name())
	requirepkg.NotEmpty(t, legacyCmd.Deprecated)
	requirepkg.NotNil(t, legacyCmd.Flags().Lookup("full-rebuild"))
	requirepkg.NotNil(t, legacyCmd.Flags().Lookup("yes"))
}

func TestListEmbeddingGenerationsIncludesActiveAndBuilding(t *testing.T) {
	db := newEmbeddingMetadataTestDB(t)

	rows, err := listEmbeddingGenerations(t.Context(), db)
	requirepkg.NoError(t, err)
	requirepkg.Len(t, rows, 2)

	assertpkg.Equal(t, vector.GenerationID(1), rows[0].ID)
	assertpkg.Equal(t, vector.GenerationActive, rows[0].State)
	assertpkg.Equal(t, int64(2), rows[0].MessageCount)
	assertpkg.Equal(t, int64(0), rows[0].PendingCount)

	assertpkg.Equal(t, vector.GenerationID(2), rows[1].ID)
	assertpkg.Equal(t, vector.GenerationBuilding, rows[1].State)
	assertpkg.Equal(t, int64(1), rows[1].PendingCount)
}

func TestActivateEmbeddingGenerationRetiresPreviousActive(t *testing.T) {
	db := newEmbeddingMetadataTestDB(t)
	ctx := t.Context()

	_, err := db.ExecContext(ctx, `DELETE FROM pending_embeddings WHERE generation_id = 2`)
	requirepkg.NoError(t, err)

	requirepkg.NoError(t, activateEmbeddingGeneration(ctx, db, 2))

	active := mustGetEmbeddingGeneration(t, ctx, db, 2)
	assertpkg.Equal(t, vector.GenerationActive, active.State)
	assertpkg.NotNil(t, active.ActivatedAt)
	assertpkg.NotNil(t, active.CompletedAt)

	retired := mustGetEmbeddingGeneration(t, ctx, db, 1)
	assertpkg.Equal(t, vector.GenerationRetired, retired.State)
	assertpkg.NotNil(t, retired.CompletedAt)
}

func TestRunEmbeddingsActivateRefusesPendingWithoutForce(t *testing.T) {
	dbPath := newEmbeddingMetadataTestDBFile(t)
	withEmbeddingCommandConfig(t, dbPath)

	oldYes := embeddingsActivateYes
	embeddingsActivateYes = true
	t.Cleanup(func() { embeddingsActivateYes = oldYes })
	cmd := embeddingsActivateCmd
	cmd.SetContext(context.Background())
	t.Cleanup(func() { cmd.SetContext(nil) })
	err := runEmbeddingsActivate(cmd, []string{"2"})

	requirepkg.Error(t, err)
	assertpkg.Contains(t, err.Error(), "pending embedding rows")
}

func TestRetireEmbeddingGenerationRefusesActiveWithoutForce(t *testing.T) {
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
	cmd.SetContext(context.Background())
	t.Cleanup(func() { cmd.SetContext(nil) })

	err := runEmbeddingsRetire(cmd, []string{"1"})
	requirepkg.Error(t, err)
	assertpkg.Contains(t, err.Error(), "active")

	embeddingsRetireForceActive = true
	requirepkg.NoError(t, runEmbeddingsRetire(cmd, []string{"1"}))

	db, err := sql.Open("sqlite3", dbPath)
	requirepkg.NoError(t, err)
	t.Cleanup(func() { requirepkg.NoError(t, db.Close()) })
	row := mustGetEmbeddingGeneration(t, t.Context(), db, 1)
	assertpkg.Equal(t, vector.GenerationRetired, row.State)
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
	(id, model, dimension, fingerprint, started_at, completed_at, activated_at, state, message_count)
VALUES
	(1, 'model', 4, ?, 100, 110, 111, 'active', 2),
	(2, 'model', 4, ?, 120, NULL, NULL, 'building', 1);
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

func mustGetEmbeddingGeneration(t *testing.T, ctx context.Context, db *sql.DB, gen vector.GenerationID) embeddingGenerationRow {
	t.Helper()
	row, err := getEmbeddingGeneration(ctx, db, gen)
	requirepkg.NoError(t, err)
	return row
}
