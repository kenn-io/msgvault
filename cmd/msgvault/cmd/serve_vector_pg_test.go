//go:build sqlite_vec && pgvector

package cmd

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

// openServePGSchema creates an isolated per-test schema on
// MSGVAULT_TEST_DB and returns a pgx *sql.DB scoped to it plus the
// schema-scoped DSN string. setupVectorFeatures detects PG from the DSN
// (mainPath) and migrates the pgvector tables into the schema via
// pgvector.Open. The schema is dropped on cleanup. Skips when
// MSGVAULT_TEST_DB is unset or not a postgres DSN.
func openServePGSchema(t *testing.T) (*sql.DB, string) {
	t.Helper()
	url := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(url, "postgres://") && !strings.HasPrefix(url, "postgresql://") {
		t.Skip("serve pgvector tests require MSGVAULT_TEST_DB to point at a PostgreSQL DSN")
	}

	buf := make([]byte, 8)
	_, err := rand.Read(buf)
	require.NoError(t, err, "random schema name")
	schemaName := "serve_vec_test_" + hex.EncodeToString(buf)

	setup, err := sql.Open("pgx", url)
	require.NoError(t, err, "open setup")
	defer func() { _ = setup.Close() }()
	_, err = setup.Exec("CREATE SCHEMA " + schemaName)
	require.NoError(t, err, "create schema")

	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	testURL := url + sep + "search_path=" + schemaName + ",public"

	db, err := sql.Open("pgx", testURL)
	require.NoError(t, err, "open")
	t.Cleanup(func() {
		_ = db.Close()
		cleanup, err := sql.Open("pgx", url)
		if err != nil {
			return
		}
		defer func() { _ = cleanup.Close() }()
		_, _ = cleanup.Exec("DROP SCHEMA " + schemaName + " CASCADE")
	})
	return db, testURL
}

// TestSetupVectorFeatures_SucceedsOnPostgres is the inverse of the old
// refusal test: with the pgvector backend compiled in, setupVectorFeatures
// must succeed against a postgres:// DSN and wire up the backend, hybrid
// engine, and enqueuer. Runs only with a live PG (MSGVAULT_TEST_DB).
func TestSetupVectorFeatures_SucceedsOnPostgres(t *testing.T) {
	savedCfg := cfg
	defer func() { cfg = savedCfg }()

	db, dsn := openServePGSchema(t)

	cfg = &config.Config{}
	cfg.Vector.Enabled = true
	cfg.Vector.Backend = "sqlite-vec" // Validate's backend gate; PG is selected from the DSN
	cfg.Vector.Embeddings.Endpoint = "http://localhost:11434/v1/embeddings"
	cfg.Vector.Embeddings.Model = "test-model"
	cfg.Vector.Embeddings.Dimension = 768
	cfg.Vector.Embeddings.BatchSize = 32

	vf, err := setupVectorFeatures(context.Background(), db, dsn, false)
	require.NoError(t, err, "setupVectorFeatures on postgres DSN must succeed with pgvector built in")
	require.NotNil(t, vf, "vectorFeatures")
	t.Cleanup(func() {
		if vf.Close != nil {
			_ = vf.Close()
		}
	})

	assert.NotNil(t, vf.Backend, "Backend wired")
	assert.NotNil(t, vf.HybridEngine, "HybridEngine wired")
	assert.NotNil(t, vf.Enqueuer, "Enqueuer wired")
	assert.NotNil(t, vf.Worker, "Worker wired")
	assert.Same(t, db, vf.VectorsDB, "PG shares the main DB handle as the vectors DB")

	// The pgvector schema was migrated into the isolated schema, so the
	// enqueuer/worker can run against it. Smoke-test that the tables exist.
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM index_generations`).Scan(&n),
		"index_generations must exist after setupVectorFeatures migrated the pgvector schema")
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM pending_embeddings`).Scan(&n),
		"pending_embeddings must exist after setupVectorFeatures migrated the pgvector schema")
}
