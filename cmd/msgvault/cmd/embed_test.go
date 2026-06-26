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
	require.NotNil(buildCmd.Flags().Lookup("backstop"))

	resumeCmd, _, err := rootCmd.Find([]string{"embeddings", "resume"})
	require.NoError(err)
	require.Equal("resume", resumeCmd.Name())
	require.Nil(resumeCmd.Flags().Lookup("full-rebuild"))
	// --backstop is also available on resume, so operators
	// can do a watermark-ignoring straggler sweep without --full-rebuild.
	require.NotNil(resumeCmd.Flags().Lookup("backstop"))

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

// TestRunEmbeddingsResume_PreservesBackstopFlag pins the resume behavior:
// resume forces incremental mode (saves/restores embedFullRebuild + embedYes) but
// must leave embedBackstop exactly as the operator set it, so
// `embeddings resume --backstop` actually runs a backstop pass.
func TestRunEmbeddingsResume_PreservesBackstopFlag(t *testing.T) {
	assert := assertpkg.New(t)

	// Save and restore all three globals so the test is hermetic.
	oldFull, oldYes, oldBackstop := embedFullRebuild, embedYes, embedBackstop
	t.Cleanup(func() { embedFullRebuild, embedYes, embedBackstop = oldFull, oldYes, oldBackstop })

	// Operator state: full-rebuild on (resume must clear it), backstop on
	// (resume must NOT touch it). Point at an empty config so the run errors
	// out early (vector disabled) without needing a real backend.
	embedFullRebuild = true
	embedYes = false
	embedBackstop = true
	oldCfg := cfg
	cfg = &config.Config{}
	t.Cleanup(func() { cfg = oldCfg })

	cmd := embeddingsResumeCmd
	oldCtx := cmd.Context()
	cmd.SetContext(context.Background())
	t.Cleanup(func() { cmd.SetContext(oldCtx) })

	// Errors because vector is not enabled — that's fine; we only assert the
	// flag-preservation contract of runEmbeddingsResume.
	_ = runEmbeddingsResume(cmd, nil)

	assert.True(embedBackstop, "resume must NOT clobber embedBackstop")
	assert.True(embedFullRebuild, "resume must restore embedFullRebuild to its prior value")
	assert.False(embedYes, "resume must restore embedYes to its prior value")
}

func TestListEmbeddingGenerationsIncludesActiveAndBuilding(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	db := newEmbeddingMetadataTestDB(t)

	// listEmbeddingGenerations reads only the generation metadata now;
	// coverage (missing count) is filled separately from the main DB via
	// fillCoverage, so it is not asserted here.
	rows, err := listEmbeddingGenerations(t.Context(), db, sqliteRebind)
	require.NoError(err)
	require.Len(rows, 2)

	assert.Equal(vector.GenerationID(1), rows[0].ID)
	assert.Equal(vector.GenerationActive, rows[0].State)
	assert.Equal(int64(2), rows[0].MessageCount)

	assert.Equal(vector.GenerationID(2), rows[1].ID)
	assert.Equal(vector.GenerationBuilding, rows[1].State)
}

// TestRunEmbeddingsActivateRefusesMissingWithoutForce verifies the CLI
// pre-flight coverage gate: activating a building generation that still
// has live messages needing embedding (embed_gen <> gen in the main DB)
// must fail without --force.
func TestRunEmbeddingsActivateRefusesMissingWithoutForce(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	dataDir := t.TempDir()
	dbPath := newEmbeddingMetadataTestDBFileAt(t, filepath.Join(dataDir, "vectors.db"))
	// Main DB with one live, unembedded message -> coverage reports
	// missing=1 for generation 2.
	seedMainDBWithLiveMessage(t, dataDir)
	withEmbeddingCommandConfigDataDir(t, dbPath, dataDir)

	oldYes := embeddingsActivateYes
	embeddingsActivateYes = true
	t.Cleanup(func() { embeddingsActivateYes = oldYes })
	cmd := embeddingsActivateCmd
	oldCtx := cmd.Context()
	cmd.SetContext(context.Background())
	t.Cleanup(func() { cmd.SetContext(oldCtx) })
	err := runEmbeddingsActivate(cmd, []string{"2"})

	require.Error(err)
	assert.Contains(err.Error(), "needing embedding")
	assert.Contains(err.Error(), "msgvault embeddings resume --backstop")
}

func TestFillCoverageUsesEmbeddingScope(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	dataDir := t.TempDir()
	dbPath := newEmbeddingMetadataTestDBFileAt(t, filepath.Join(dataDir, "vectors.db"))
	seedMainDBWithScopedCoverageMessages(t, dataDir)
	withEmbeddingCommandConfigDataDir(t, dbPath, dataDir)
	cfg.Vector.Embed.Scope.MessageTypes = []string{"sms"}

	row := embeddingGenerationRow{ID: 2}
	require.NoError(fillCoverage(t.Context(), &row))

	assert.Equal(int64(1), row.LiveCount)
	assert.Equal(int64(0), row.MissingCount)
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
	return newEmbeddingMetadataTestDBFileAt(t, filepath.Join(t.TempDir(), "vectors.db"))
}

// newEmbeddingMetadataTestDBFileAt creates a vectors.db with just the
// index_generations metadata (no pending_embeddings — coverage now lives
// in the main DB) at the given path.
func newEmbeddingMetadataTestDBFileAt(t *testing.T, path string) string {
	t.Helper()
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
`)
	requirepkg.NoError(t, err)

	fp := newTestConfigForFingerprint("").Vector.GenerationFingerprint()
	_, err = db.Exec(`
INSERT INTO index_generations
	(id, model, dimension, fingerprint, started_at, seeded_at, completed_at, activated_at, state, message_count)
VALUES
	(1, 'model', 4, ?, 100, 101, 110, 111, 'active', 2),
	(2, 'model', 4, ?, 120, 121, NULL, NULL, 'building', 1);
`, fp, fp)
	requirepkg.NoError(t, err)
	return path
}

// seedMainDBWithLiveMessage creates a main msgvault.db in dataDir with one
// live message whose embed_gen is NULL — i.e. it reads as "missing" for
// every generation, so the coverage gate refuses activation.
func seedMainDBWithLiveMessage(t *testing.T, dataDir string) {
	t.Helper()
	s, err := store.Open(filepath.Join(dataDir, "msgvault.db"))
	requirepkg.NoError(t, err)
	defer func() { requirepkg.NoError(t, s.Close()) }()
	requirepkg.NoError(t, s.InitSchema())
	_, err = s.DB().Exec(`
INSERT INTO sources (id, source_type, identifier) VALUES (1, 'gmail', 'me@example.com');
INSERT INTO conversations (id, source_id, conversation_type) VALUES (1, 1, 'email_thread');
INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, embed_gen) VALUES (1, 1, 1, 'm1', 'email', NULL);
`)
	requirepkg.NoError(t, err)
}

func seedMainDBWithScopedCoverageMessages(t *testing.T, dataDir string) {
	t.Helper()
	s, err := store.Open(filepath.Join(dataDir, "msgvault.db"))
	requirepkg.NoError(t, err)
	defer func() { requirepkg.NoError(t, s.Close()) }()
	requirepkg.NoError(t, s.InitSchema())
	_, err = s.DB().Exec(`
INSERT INTO sources (id, source_type, identifier) VALUES (1, 'gmail', 'me@example.com');
INSERT INTO conversations (id, source_id, conversation_type) VALUES (1, 1, 'email_thread'), (2, 1, 'sms_thread');
INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, embed_gen) VALUES
	(1, 1, 1, 'email-missing', 'email', NULL),
	(2, 2, 1, 'sms-stamped', 'sms', 2);
`)
	requirepkg.NoError(t, err)
}

func seedMainDBWithScopedFullCoverageMessages(t *testing.T, dataDir string) {
	t.Helper()
	s, err := store.Open(filepath.Join(dataDir, "msgvault.db"))
	requirepkg.NoError(t, err)
	defer func() { requirepkg.NoError(t, s.Close()) }()
	requirepkg.NoError(t, s.InitSchema())
	_, err = s.DB().Exec(`
INSERT INTO sources (id, source_type, identifier) VALUES (1, 'gmail', 'me@example.com');
INSERT INTO conversations (id, source_id, conversation_type) VALUES (1, 1, 'email_thread'), (2, 1, 'sms_thread');
INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, embed_gen) VALUES
	(1, 1, 1, 'email-stamped', 'email', 2),
	(2, 2, 1, 'sms-stamped', 'sms', 2);
`)
	requirepkg.NoError(t, err)
}

func withEmbeddingCommandConfig(t *testing.T, vecPath string) {
	t.Helper()
	oldCfg := cfg
	cfg = newTestConfigForFingerprint(vecPath)
	t.Cleanup(func() { cfg = oldCfg })
}

// withEmbeddingCommandConfigDataDir is like withEmbeddingCommandConfig but
// also sets Data.DataDir so DatabaseDSN() resolves to a real main DB (used
// by the coverage gate).
func withEmbeddingCommandConfigDataDir(t *testing.T, vecPath, dataDir string) {
	t.Helper()
	oldCfg := cfg
	c := newTestConfigForFingerprint(vecPath)
	c.Data.DataDir = dataDir
	cfg = c
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
