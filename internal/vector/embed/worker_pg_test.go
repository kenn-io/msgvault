//go:build pgvector

package embed

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector/pgvector"
)

// pgFakeEmbeddingClient returns one deterministic, non-zero vector per
// input. Defined locally because the sqlite_vec testsupport's
// fakeEmbeddingClient is behind a different build tag.
type pgFakeEmbeddingClient struct{ dim int }

func (c *pgFakeEmbeddingClient) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i := range inputs {
		v := make([]float32, c.dim)
		v[0] = float32(len(inputs[i])%c.dim + 1)
		out[i] = v
	}
	return out, nil
}

// openPGWorkerDB stands up a per-test schema on MSGVAULT_TEST_DB with the
// minimal main-schema tables embedBatch reads (messages + message_bodies,
// including the deleted_* columns LiveMessagesWhere references) and seeds
// n live messages. It returns the *sql.DB; cleanup drops the schema.
func openPGWorkerDB(t *testing.T, n int) *sql.DB {
	t.Helper()
	url := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(url, "postgres://") && !strings.HasPrefix(url, "postgresql://") {
		t.Skip("pgvector worker tests require MSGVAULT_TEST_DB to point at a PostgreSQL DSN")
	}

	buf := make([]byte, 8)
	_, err := rand.Read(buf)
	require.NoError(t, err, "random schema name")
	schemaName := "embed_w_test_" + hex.EncodeToString(buf)

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
		_, _ = cleanup.Exec("DROP SCHEMA " + schemaName + " CASCADE")
	})

	_, err = db.Exec(`
		CREATE TABLE messages (
			id BIGINT PRIMARY KEY,
			subject TEXT,
			deleted_at TIMESTAMPTZ,
			deleted_from_source_at TIMESTAMPTZ
		);
		CREATE TABLE message_bodies (
			message_id BIGINT PRIMARY KEY,
			body_text TEXT,
			body_html TEXT
		);`)
	require.NoError(t, err, "create main schema")

	ctx := context.Background()
	for i := 1; i <= n; i++ {
		_, err := db.ExecContext(ctx,
			`INSERT INTO messages (id, subject) VALUES ($1, $2)`, i, fmt.Sprintf("msg %d", i))
		require.NoError(t, err, "insert message")
		_, err = db.ExecContext(ctx,
			`INSERT INTO message_bodies (message_id, body_text) VALUES ($1, $2)`, i, fmt.Sprintf("body %d", i))
		require.NoError(t, err, "insert body")
	}
	return db
}

// TestWorkerPG_RunOnce_EndToEnd drives the full embed BUILD pipeline
// against pgx: CreateGeneration seeds pending_embeddings from messages,
// then RunOnce claims, fetches bodies via embedBatch's IN(...) query,
// embeds, upserts, and completes. This exercises the $N-placeholder path
// in embedBatch — before the rebind fix it failed with pgx error 42601
// ("syntax error at or near ','") because embedBatch emitted literal `?`.
func TestWorkerPG_RunOnce_EndToEnd(t *testing.T) {
	ctx := context.Background()
	const n = 5
	db := openPGWorkerDB(t, n)

	backend, err := pgvector.Open(ctx, pgvector.Options{DB: db, Dimension: 4})
	require.NoError(t, err, "pgvector.Open")
	t.Cleanup(func() { _ = backend.Close() })

	gen, err := backend.CreateGeneration(ctx, "fake", 4, "")
	require.NoError(t, err, "CreateGeneration")

	// Sanity: seeding put one pending row per live message.
	var pending int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = $1`, int64(gen)).Scan(&pending))
	require.Equal(t, n, pending, "pending seeded from messages")

	worker := NewWorker(WorkerDeps{
		Backend:   backend,
		VectorsDB: db,
		MainDB:    db,
		Client:    &pgFakeEmbeddingClient{dim: 4},
		Rebind:    (&store.PostgreSQLDialect{}).Rebind,
		BatchSize: 2, // force multiple claim/embedBatch rounds
	})

	res, err := worker.RunOnce(ctx, gen)
	require.NoError(t, err, "RunOnce must not error on pgx")
	assert.Equal(t, n, res.Succeeded, "all messages embedded")
	assert.Equal(t, 0, res.Failed, "no failures")

	// Queue fully drained.
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = $1`, int64(gen)).Scan(&pending))
	assert.Equal(t, 0, pending, "pending drained after RunOnce")

	// Embeddings landed, one row per message.
	var embedded int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = $1`, int64(gen)).Scan(&embedded))
	assert.Equal(t, n, embedded, "one embedding row per message")
}

// TestWorkerPG_EmbedBatch_RebindsINClause targets embedBatch directly:
// it must rebind the WHERE id IN (...) placeholders to $N so the pgx
// driver accepts the query. A non-rebinding embedBatch returns a 42601
// syntax error here; the assertion is simply that the fetch succeeds and
// returns the seeded messages.
func TestWorkerPG_EmbedBatch_RebindsINClause(t *testing.T) {
	ctx := context.Background()
	db := openPGWorkerDB(t, 3)

	backend, err := pgvector.Open(ctx, pgvector.Options{DB: db, Dimension: 4})
	require.NoError(t, err, "pgvector.Open")
	t.Cleanup(func() { _ = backend.Close() })

	w := NewWorker(WorkerDeps{
		Backend:   backend,
		VectorsDB: db,
		MainDB:    db,
		Client:    &pgFakeEmbeddingClient{dim: 4},
		Rebind:    (&store.PostgreSQLDialect{}).Rebind,
	})

	eb, err := w.embedBatch(ctx, []int64{1, 2, 3})
	require.NoError(t, err, "embedBatch must rebind ? to $N for pgx")
	assert.Len(t, eb.embeddedIDs, 3, "all three messages fetched and embedded")
	assert.Len(t, eb.chunks, 3, "one chunk per short message")
	assert.Empty(t, eb.missing, "no missing messages")
	assert.Empty(t, eb.empty, "no empty messages")
	// Every chunk carries a non-zero vector of the generation's dim.
	for _, c := range eb.chunks {
		assert.Len(t, c.Vector, 4)
	}
}
