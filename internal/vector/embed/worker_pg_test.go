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
type pgFakeEmbeddingClient struct {
	dim int
}

func (c *pgFakeEmbeddingClient) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i := range inputs {
		v := make([]float32, c.dim)
		v[0] = float32(len(inputs[i])%c.dim + 1)
		out[i] = v
	}
	return out, nil
}

// pgWorkStore is a minimal WorkStore over the PG test schema, mirroring
// store.ScanForEmbedding / store.SetEmbedGen with $N placeholders.
type pgWorkStore struct{ db *sql.DB }

func (s *pgWorkStore) ScanForEmbedding(ctx context.Context, target int64, afterID int64, limit int) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM messages
		  WHERE (embed_gen IS NULL OR embed_gen <> $1)
		    AND deleted_at IS NULL AND deleted_from_source_at IS NULL
		    AND id > $2
		  ORDER BY id LIMIT $3`, target, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *pgWorkStore) SetEmbedGen(ctx context.Context, ids []int64, target int64) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE messages SET embed_gen = $1 WHERE id = ANY($2::bigint[])`, target, int64ArrayLiteral(ids))
	return err
}

// SetEmbedGenIfUnchanged mirrors store.Store.SetEmbedGenIfUnchanged on the
// PG test schema: one transaction of per-row optimistic-CAS stamps gated on
// last_modified. Returns the ids whose UPDATE matched 0 rows (CAS misses).
func (s *pgWorkStore) SetEmbedGenIfUnchanged(ctx context.Context, items []store.EmbedGenStamp, target int64) (missed []int64, covered int, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	for _, it := range items {
		res, err := tx.ExecContext(ctx,
			`UPDATE messages SET embed_gen = $1 WHERE id = $2 AND last_modified = $3`,
			target, it.ID, it.LastModified)
		if err != nil {
			return nil, 0, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, 0, err
		}
		if n == 0 {
			missed = append(missed, it.ID)
		} else {
			covered++
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, err
	}
	return missed, covered, nil
}

func int64ArrayLiteral(ids []int64) string {
	var sb strings.Builder
	sb.WriteByte('{')
	for i, id := range ids {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "%d", id)
	}
	sb.WriteByte('}')
	return sb.String()
}

func pgCountMissing(t *testing.T, db *sql.DB, gen int64) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM messages
		  WHERE (embed_gen IS NULL OR embed_gen <> $1)
		    AND deleted_at IS NULL AND deleted_from_source_at IS NULL`, gen).Scan(&n))

	return n
}

// openPGWorkerDB stands up a per-test schema on MSGVAULT_TEST_DB with the
// minimal main-schema tables embedBatch reads (messages + message_bodies,
// including embed_gen and the deleted_* columns LiveMessagesWhere
// references) and seeds n live messages. Returns the *sql.DB; cleanup
// drops the schema.
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
			deleted_from_source_at TIMESTAMPTZ,
			embed_gen BIGINT,
			last_modified TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE message_bodies (
			message_id BIGINT PRIMARY KEY,
			body_text TEXT,
			body_html TEXT
		);
		CREATE OR REPLACE FUNCTION set_messages_last_modified() RETURNS trigger AS $f$
		BEGIN
			NEW.last_modified := CURRENT_TIMESTAMP;
			RETURN NEW;
		END;
		$f$ LANGUAGE plpgsql;
		CREATE TRIGGER trg_messages_last_modified
			BEFORE UPDATE ON messages FOR EACH ROW
			WHEN (OLD.last_modified IS NOT DISTINCT FROM NEW.last_modified)
			EXECUTE FUNCTION set_messages_last_modified();
		CREATE OR REPLACE FUNCTION bump_message_last_modified() RETURNS trigger AS $f$
		BEGIN
			UPDATE messages SET last_modified = CURRENT_TIMESTAMP WHERE id = NEW.message_id;
			RETURN NEW;
		END;
		$f$ LANGUAGE plpgsql;
		CREATE TRIGGER trg_message_bodies_last_modified
			AFTER INSERT OR UPDATE ON message_bodies FOR EACH ROW
			EXECUTE FUNCTION bump_message_last_modified();`)
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

// TestWorkerPG_RunOnce_EndToEnd drives the full scan-and-fill pipeline
// against pgx: the worker scans messages.embed_gen, fetches bodies via
// embedBatch's IN(...) query (rebound to $N), embeds, upserts, and stamps
// embed_gen. Coverage must reach zero.
func TestWorkerPG_RunOnce_EndToEnd(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	const n = 5
	db := openPGWorkerDB(t, n)

	backend, err := pgvector.Open(ctx, pgvector.Options{DB: db, Dimension: 4})
	require.NoError(
		err, "pgvector.Open")

	t.Cleanup(func() { _ = backend.Close() })

	gen, err := backend.CreateGeneration(ctx, "fake", 4, "")
	require.NoError(
		err, "CreateGeneration")

	require. // Everything reads as missing before the run.
			Equal(n, pgCountMissing(t, db, int64(gen)), "missing before run")

	worker := NewWorker(WorkerDeps{
		Backend:          backend,
		VectorsDB:        db,
		MainDB:           db,
		Store:            &pgWorkStore{db: db},
		Client:           &pgFakeEmbeddingClient{dim: 4},
		Rebind:           (&store.PostgreSQLDialect{}).Rebind,
		LastModifiedExpr: "m.last_modified",
		BatchSize:        2, // force multiple scan/embedBatch rounds
		Recorder:         newTestOperationRecorder(),
	})

	res, err := worker.RunOnce(ctx, gen, testEmbeddingPassScope())
	require.NoError(
		err, "RunOnce must not error on pgx")

	assert.Equal(n, res.Succeeded, "all messages embedded")
	assert.Equal(0, res.Failed, "no failures")
	assert. // Coverage complete after the run.
		Equal(0, pgCountMissing(t, db, int64(gen)), "missing after run")

	// Embeddings landed, one row per message.
	var embedded int
	require.NoError(db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = $1`, int64(gen)).Scan(&embedded))
	assert.Equal(n, embedded, "one embedding row per message")
}

// TestWorkerPG_EmbedBatch_RebindsINClause targets embedBatch directly: it
// must rebind the WHERE id IN (...) placeholders to $N so the pgx driver
// accepts the query.
func TestWorkerPG_EmbedBatch_RebindsINClause(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	db := openPGWorkerDB(t, 3)

	backend, err := pgvector.Open(ctx, pgvector.Options{DB: db, Dimension: 4})
	require.NoError(
		err, "pgvector.Open")

	t.Cleanup(func() { _ = backend.Close() })

	w := NewWorker(WorkerDeps{
		Backend:          backend,
		VectorsDB:        db,
		MainDB:           db,
		Store:            &pgWorkStore{db: db},
		Client:           &pgFakeEmbeddingClient{dim: 4},
		Rebind:           (&store.PostgreSQLDialect{}).Rebind,
		LastModifiedExpr: "m.last_modified",
	})

	eb, err := w.embedBatch(ctx, []int64{1, 2, 3})
	require.NoError(
		err, "embedBatch must rebind ? to $N for pgx")

	assert.Len(eb.embeddedIDs, 3, "all three messages fetched and embedded")
	assert.Len(eb.chunks, 3, "one chunk per short message")
	assert.Empty(eb.missing, "no missing messages")
	assert.Empty(eb.empty, "no empty messages")
	for _, c := range eb.chunks {
		assert.Len(c.Vector, 4)
	}
}
