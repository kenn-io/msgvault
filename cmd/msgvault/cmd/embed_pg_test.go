//go:build sqlite_vec && pgvector

package cmd

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"strconv"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/pgvector"
)

// countEmbeddingRowsPG returns the number of embedding rows for a generation
// in a live PG database. Used by the CLI delete-on-retire regression tests.
func countEmbeddingRowsPG(t *testing.T, db *sql.DB, gen vector.GenerationID) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = $1`, int64(gen)).Scan(&n),
		"count embedding rows for generation %d", gen)
	return n
}

// seedGenWithEmbeddingsPG creates a building generation and upserts one chunk
// per supplied message id (dim 4). The consuming tests force-activate/retire
// (force=true), bypassing the coverage gate, so no embed_gen stamping is
// needed to make the management commands treat it as finished. Returns the
// generation id.
func seedGenWithEmbeddingsPG(t *testing.T, pgb *pgvector.Backend, ids ...int64) vector.GenerationID {
	t.Helper()
	ctx := context.Background()
	// No messages rows are inserted: embeddings.message_id has no FK to
	// messages, and the consuming tests force-activate/retire (force=true),
	// bypassing the coverage gate, so no live messages are needed. (The full
	// schema's messages.id is GENERATED ALWAYS AS IDENTITY, so an explicit-id
	// insert would be rejected anyway.)
	gen, err := pgb.CreateGeneration(ctx, "test-model", 4, "test-model:4")
	require.NoError(t, err, "CreateGeneration")
	chunks := make([]vector.Chunk, 0, len(ids))
	for i, id := range ids {
		v := make([]float32, 4)
		v[i%4] = 1
		chunks = append(chunks, vector.Chunk{MessageID: id, ChunkIndex: 0, Vector: v})
	}
	require.NoError(t, pgb.Upsert(ctx, gen, chunks), "Upsert")
	// The consuming tests force-activate/retire (force=true), bypassing the
	// coverage gate, so no embed_gen stamping is needed here.
	return gen
}

// TestRunEmbeddingsRetire_PG_DeletesEmbeddings pins the cf-2 fix: the CLI
// `embeddings retire` path must delete the retired generation's embeddings on
// PostgreSQL (routing through backend.RetireGeneration), not merely flip
// index_generations.state. Before the fix, runEmbeddingsRetire called the
// raw-SQL helper that only updated state, leaving the retired gen's vectors in
// the shared HNSW graph.
func TestRunEmbeddingsRetire_PG_DeletesEmbeddings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	pgb, rebind, dsn := openEmbedManagePGDB(t)
	ctx := context.Background()
	db := pgb.DB()

	gen := seedGenWithEmbeddingsPG(t, pgb, 1, 2, 3)
	require.Equal(3, countEmbeddingRowsPG(t, db, gen), "precondition: vectors present before retire")

	savedCfg := cfg
	savedYes, savedForce := embeddingsRetireYes, embeddingsRetireForceActive
	defer func() {
		cfg = savedCfg
		embeddingsRetireYes, embeddingsRetireForceActive = savedYes, savedForce
	}()
	cfg = &config.Config{}
	cfg.Data.DatabaseURL = dsn
	cfg.Vector.Embeddings.Dimension = 4
	embeddingsRetireYes = true
	embeddingsRetireForceActive = true

	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	var out bytes.Buffer
	cmd.SetOut(&out)
	require.NoError(
		runEmbeddingsRetire(cmd, []string{strconv.FormatInt(int64(gen), 10)}),
		"runEmbeddingsRetire on PG")

	g, err := getEmbeddingGeneration(ctx, db, rebind, gen)
	require.NoError(
		err, "getEmbeddingGeneration after retire")

	assert.Equal(vector.GenerationRetired, g.State, "generation must be retired")
	assert.Equal(0, countEmbeddingRowsPG(t, db, gen),
		"CLI retire must DELETE the retired generation's embeddings on PG (cf-2)")
}

// TestRunEmbeddingsActivate_PG_AutoRetireDeletesPrevious pins the activate arm
// of cf-2: activating a new generation auto-retires the previously-active one,
// and that auto-retire must delete the demoted generation's embeddings on PG.
func TestRunEmbeddingsActivate_PG_AutoRetireDeletesPrevious(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	pgb, rebind, dsn := openEmbedManagePGDB(t)
	ctx := context.Background()
	db := pgb.DB()

	genA := seedGenWithEmbeddingsPG(t, pgb, 1, 2)
	require.NoError(
		pgb.ActivateGeneration(ctx, genA, true), "activate A directly")

	require.Equal(2, countEmbeddingRowsPG(t, db, genA), "A populated before re-embed")

	genB := seedGenWithEmbeddingsPG(t, pgb, 3, 4)

	savedCfg := cfg
	savedYes, savedForce := embeddingsActivateYes, embeddingsActivateForce
	defer func() {
		cfg = savedCfg
		embeddingsActivateYes, embeddingsActivateForce = savedYes, savedForce
	}()
	cfg = &config.Config{}
	cfg.Data.DatabaseURL = dsn
	cfg.Vector.Embeddings.Dimension = 4
	cfg.Vector.Embeddings.Model = "test-model"
	embeddingsActivateYes = true
	embeddingsActivateForce = true // skip fingerprint/seeded gating in the run func

	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	var out bytes.Buffer
	cmd.SetOut(&out)
	require.NoError(
		runEmbeddingsActivate(cmd, []string{strconv.FormatInt(int64(genB), 10)}),
		"runEmbeddingsActivate on PG (auto-retires A)")

	gA, err := getEmbeddingGeneration(ctx, db, rebind, genA)
	require.NoError(
		err, "lookup A after activate B")

	assert.Equal(vector.GenerationRetired, gA.State, "A must be auto-retired")
	assert.Equal(0, countEmbeddingRowsPG(t, db, genA),
		"auto-retired generation A's embeddings must be DELETED via CLI activate (cf-2)")
	assert.Equal(2, countEmbeddingRowsPG(t, db, genB), "newly-activated B's rows untouched")
}

// openEmbedManagePGDB opens a per-test isolated PG schema, migrates the
// pgvector tables into it (via pgvector.Open), and returns the pgvector
// backend, the PG rebind func, and the schema-scoped DSN. The underlying
// *sql.DB is reachable via the returned backend's DB() method. The DSN lets
// tests that exercise the CLI run functions point cfg.Data.DatabaseURL at the
// SAME isolated schema the backend uses. Skips when MSGVAULT_TEST_DB is unset.
func openEmbedManagePGDB(t *testing.T) (*pgvector.Backend, func(string) string, string) {
	t.Helper()
	_, dsn := openServePGSchema(t)
	ctx := context.Background()

	st, err := store.Open(dsn)
	require.NoError(t, err, "store.Open")
	t.Cleanup(func() { _ = st.Close() })

	// Mirror production: the CLI embeddings commands (runEmbed,
	// runEmbeddingsRetire/Activate/List) all run store.InitSchema BEFORE
	// opening the pgvector backend. InitSchema creates the full messages table
	// (with embed_gen) and applied_migrations, so the backend's open-time reset/
	// backfill have the columns they touch. A minimal hand-rolled messages
	// table omitting embed_gen diverges from production and breaks both the
	// open-time reset and the activate/retire coverage gates (which reference
	// embed_gen even under --force).
	require.NoError(t, st.InitSchema(), "InitSchema")

	pgb, err := pgvector.Open(ctx, pgvector.Options{
		DB:        st.DB(),
		Dimension: 4,
	})
	require.NoError(t, err, "pgvector.Open")
	t.Cleanup(func() { _ = pgb.Close() })

	return pgb, (&store.PostgreSQLDialect{}).Rebind, dsn
}

// TestListEmbeddingGenerations_PG exercises listEmbeddingGenerations through
// the PG rebind path against a live PostgreSQL database. Validates that the
// PG placeholder rebind and boolean-placeholder behaviour work correctly.
func TestListEmbeddingGenerations_PG(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	pgb, rebind, _ := openEmbedManagePGDB(t)
	ctx := context.Background()
	db := pgb.DB()

	// Start with an empty database — list must return an empty slice, not error.
	rows, err := listEmbeddingGenerations(ctx, db, rebind)
	require.NoError(
		err, "listEmbeddingGenerations on empty PG DB must not error")

	assert.Empty(rows, "no generations yet")

	// Create a generation so list returns something.
	gen, err := pgb.CreateGeneration(ctx, "test-model", 4, "test-model:4")
	require.NoError(
		err, "CreateGeneration")

	rows, err = listEmbeddingGenerations(ctx, db, rebind)
	require.NoError(
		err, "listEmbeddingGenerations after CreateGeneration")

	require.Len(rows, 1, "one generation")
	assert.Equal(gen, rows[0].ID)
	assert.Equal(vector.GenerationBuilding, rows[0].State)
	assert.Equal("test-model", rows[0].Model)
}

// TestOpenEmbeddingsMetadataDB_PG exercises the real openEmbeddingsMetadataDB
// helper against a live PG DSN. It pins the PG branch's contract: the helper
// routes through store.OpenPostgresDB (not raw sql.Open), returns the PG
// rebind (not the SQLite identity rebind), and yields a live handle that the
// production query helpers can use. The cfg-global swap mirrors
// TestSetupVectorFeatures_SucceedsOnPostgres.
func TestOpenEmbeddingsMetadataDB_PG(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	// Stand up an isolated schema and migrate the pgvector metadata tables
	// into it so the helper's existence pre-check passes.
	db, dsn := openServePGSchema(t)
	ctx := context.Background()
	require.NoError(
		pgvector.Migrate(ctx, db, 4, false), "pgvector.Migrate")

	savedCfg := cfg
	defer func() { cfg = savedCfg }()
	cfg = &config.Config{}
	cfg.Data.DatabaseURL = dsn

	mdb, rebind, closeDB, err := openEmbeddingsMetadataDB(ctx)
	require.NoError(
		err, "openEmbeddingsMetadataDB on a migrated PG schema must succeed")

	require.NotNil(mdb, "metadata DB handle")
	require.NotNil(closeDB, "close callback")
	defer closeDB()
	assert. // The PG branch must return the PG rebind, not the SQLite identity rebind.
		Equal("$1", rebind("?"), "PG rebind must convert ? to $1")

	// The handle must be live and usable by the production query helper.
	rows, err := listEmbeddingGenerations(ctx, mdb, rebind)
	require.NoError(
		err, "listEmbeddingGenerations via openEmbeddingsMetadataDB handle")

	assert.Empty(rows, "freshly migrated schema has no generations yet")
}

// TestOpenEmbeddingsMetadataDB_PG_FriendlyErrorWhenUnmigrated pins
// cmd-glue-stubs-2: on a PG deployment where the vector schema has not been
// migrated (no embed run yet), openEmbeddingsMetadataDB must return a
// friendly, build-pointing error rather than leaking a raw
// `relation "index_generations" does not exist (SQLSTATE 42P01)`.
func TestOpenEmbeddingsMetadataDB_PG_FriendlyErrorWhenUnmigrated(t *testing.T) {
	// Use a search_path scoped to ONLY the fresh isolated schema (no
	// "public") so to_regclass cannot resolve against tables that prior
	// non-isolated test runs may have left in public — the schema genuinely
	// has no metadata tables, matching an un-migrated PG deployment.
	dsn := openEmptyPGSchemaSolo(t)

	savedCfg := cfg
	defer func() { cfg = savedCfg }()
	cfg = &config.Config{}
	cfg.Data.DatabaseURL = dsn

	_, _, closeDB, err := openEmbeddingsMetadataDB(context.Background())
	if closeDB != nil {
		closeDB()
	}
	require.Error(t, err, "must error when metadata tables are absent")
	assert.Contains(t, err.Error(), "embeddings build",
		"error must point the user at \"msgvault embeddings build\"")
	assert.NotContains(t, err.Error(), "42P01",
		"raw PostgreSQL SQLSTATE must not leak to the user")
}

// openEmptyPGSchemaSolo creates an isolated empty schema and returns a DSN
// whose search_path is that schema ALONE (no public). Used by the
// un-migrated negative test so to_regclass('index_generations') resolves to
// NULL regardless of what prior test runs left in public. Skips when
// MSGVAULT_TEST_DB is not a PostgreSQL DSN.
func openEmptyPGSchemaSolo(t *testing.T) string {
	t.Helper()
	url := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(url, "postgres://") && !strings.HasPrefix(url, "postgresql://") {
		t.Skip("requires MSGVAULT_TEST_DB to point at a PostgreSQL DSN")
	}
	buf := make([]byte, 8)
	_, err := rand.Read(buf)
	require.NoError(t, err, "random schema name")
	schemaName := "embed_solo_test_" + hex.EncodeToString(buf)

	setup, err := sql.Open("pgx", url)
	require.NoError(t, err, "open setup")
	defer func() { _ = setup.Close() }()
	_, err = setup.Exec("CREATE SCHEMA " + schemaName)
	require.NoError(t, err, "create schema")
	t.Cleanup(func() {
		cleanup, err := sql.Open("pgx", url)
		if err != nil {
			return
		}
		defer func() { _ = cleanup.Close() }()
		_, _ = cleanup.Exec("DROP SCHEMA " + schemaName + " CASCADE")
	})

	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	return url + sep + "search_path=" + schemaName
}
