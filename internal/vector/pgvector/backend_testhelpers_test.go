//go:build pgvector

package pgvector

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

// testDBURL returns the value of MSGVAULT_TEST_DB if it names a
// PostgreSQL DSN; otherwise it skips the calling test. pgvector tests
// require a live Postgres with the pgvector extension installed — they
// cannot fall back to an in-memory database the way sqlitevec tests do.
func testDBURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(url, "postgres://") && !strings.HasPrefix(url, "postgresql://") {
		t.Skip("pgvector tests require MSGVAULT_TEST_DB to point at a PostgreSQL DSN")
	}
	return url
}

// openPGTestDB connects to MSGVAULT_TEST_DB on its own per-test schema
// and applies the minimum main-schema scaffolding the backend's
// SELECT/JOIN paths exercise. Returns the *sql.DB plus a cleanup that
// drops the schema; the *sql.DB is closed via t.Cleanup as well.
//
// We deliberately stand up a minimal messages schema rather than
// importing the full store.InitSchema because the vector backend only
// touches a small slice of the main tables and pulling in the full
// schema entangles every test with the rest of the codebase's
// migrations.
func openPGTestDB(t *testing.T) *sql.DB {
	t.Helper()
	url := testDBURL(t)

	buf := make([]byte, 8)
	_, err := rand.Read(buf)
	require.NoError(t, err, "random schema name")
	schemaName := "pgvec_test_" + hex.EncodeToString(buf)

	setup, err := sql.Open("pgx", url)
	require.NoError(t, err, "open setup")
	defer func() { _ = setup.Close() }()
	_, err = setup.Exec("CREATE SCHEMA " + schemaName)
	require.NoError(t, err, "create schema")

	testURL := url
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	// Search the per-test schema first, then the shared "public" schema
	// where pgvector's extension objects live. Without "public" in the
	// path the per-test connection can't resolve the `vector` type.
	testURL += sep + "search_path=" + schemaName + ",public"

	db, err := sql.Open("pgx", testURL)
	require.NoError(t, err, "open")

	t.Cleanup(func() {
		_ = db.Close()
		cleanup, err := sql.Open("pgx", url)
		if err != nil {
			return
		}
		defer func() { _ = cleanup.Close() }()
		_, _ = cleanup.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE", schemaName))
	})

	testSetupPGSchema(t, db)
	return db
}

// testSetupPGSchema creates the minimal main-schema tables that all PG
// test files need. Called by openPGTestDB so that every caller gets a
// consistent schema; fused tests may extend it with extra columns via
// ALTER TABLE IF NOT EXISTS / CREATE TABLE IF NOT EXISTS.
func testSetupPGSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`
		CREATE TABLE messages (
			id BIGINT PRIMARY KEY,
			source_id BIGINT,
			sender_id BIGINT,
			subject TEXT,
			message_type TEXT NOT NULL DEFAULT 'email',
			has_attachments BOOLEAN DEFAULT FALSE,
			size_estimate BIGINT,
			sent_at TIMESTAMPTZ,
			deleted_at TIMESTAMPTZ,
			deleted_from_source_at TIMESTAMPTZ,
			embed_gen BIGINT
		);
		CREATE TABLE message_recipients (
			id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			message_id BIGINT NOT NULL,
			recipient_type TEXT NOT NULL,
			participant_id BIGINT NOT NULL
		);
		CREATE TABLE message_labels (
			message_id BIGINT NOT NULL,
			label_id BIGINT NOT NULL,
			PRIMARY KEY (message_id, label_id)
		);`)
	require.NoError(t, err, "create main schema")
}

// seedOneMessage inserts a single live message (id=1) into the main
// schema, mirroring the SQLite testhelper of the same shape.
func seedOneMessage(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO messages (id) VALUES (1)`)
	require.NoError(t, err, "seed message")
}

// newBackendForTest opens a per-test database with one live message
// and returns a Backend ready for assertions.
func newBackendForTest(t *testing.T) (*Backend, context.Context, *sql.DB) {
	t.Helper()
	db := openPGTestDB(t)
	seedOneMessage(t, db)
	ctx := context.Background()
	b, err := Open(ctx, Options{DB: db, Dimension: 768})
	require.NoError(t, err, "Open")
	t.Cleanup(func() { _ = b.Close() })
	return b, ctx, db
}

// unitVec returns a unit vector of the given dimension with 1.0 at
// position axis and 0.0 elsewhere — the building block for similarity
// assertions where we know which message should rank first.
func unitVec(dim, axis int) []float32 {
	v := make([]float32, dim)
	v[axis] = 1
	return v
}

// seedAndEmbed inserts any missing message rows, creates a building
// generation sized to the first vector, upserts every supplied vector
// as a chunk, and stamps messages.embed_gen for those rows so the caller
// sees the "fully embedded" end state (coverage complete). Mirrors the
// sqlitevec helper of the same name.
func seedAndEmbed(t *testing.T, b *Backend, db *sql.DB, vecs map[int64][]float32) vector.GenerationID {
	t.Helper()
	require.NotEmpty(t, vecs, "seedAndEmbed: no vectors supplied")
	ctx := context.Background()

	ids := make([]int64, 0, len(vecs))
	for id := range vecs {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	expectedDim := len(vecs[ids[0]])
	for _, id := range ids {
		v := vecs[id]
		require.Lenf(t, v, expectedDim, "seedAndEmbed: vector for msg %d has %d dims, want %d", id, len(v), expectedDim)
	}

	for _, id := range ids {
		_, err := db.ExecContext(ctx,
			`INSERT INTO messages (id) VALUES ($1) ON CONFLICT DO NOTHING`, id)
		require.NoErrorf(t, err, "seed message %d", id)
	}

	gid, err := b.CreateGeneration(ctx, "m", expectedDim, "")
	require.NoError(t, err, "CreateGeneration")

	chunks := make([]vector.Chunk, 0, len(ids))
	for _, id := range ids {
		chunks = append(chunks, vector.Chunk{MessageID: id, Vector: vecs[id]})
	}
	require.NoError(t, b.Upsert(ctx, gid, chunks), "Upsert")

	for _, id := range ids {
		_, err = db.ExecContext(ctx,
			`UPDATE messages SET embed_gen = $1 WHERE id = $2`, int64(gid), id)
		require.NoErrorf(t, err, "stamp embed_gen for msg %d", id)
	}
	return gid
}
