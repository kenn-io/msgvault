//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

func TestMigrate_FreshAndIdempotent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "vectors.db")

	db := openTestDB(t, path)
	t.Cleanup(func() { _ = db.Close() })

	requirepkg.NoError(t, Migrate(ctx, db, 768), "first migrate")

	for _, tbl := range []string{
		"index_generations", "embeddings", "embed_runs",
		"pending_embeddings", "vectors_vec_d768", "schema_version",
	} {
		var name string
		err := db.QueryRow(`SELECT name FROM sqlite_master WHERE name = ?`, tbl).Scan(&name)
		assertpkg.NoErrorf(t, err, "table %s missing", tbl)
	}

	// Idempotent: running again must not error.
	requirepkg.NoError(t, Migrate(ctx, db, 768), "second migrate")
}

// TestMigrate_LegacyToChunked builds a pre-chunking vectors.db
// (embeddings keyed by (generation_id, message_id), vec0 with
// `message_id INTEGER PRIMARY KEY`), runs Migrate, and asserts:
//
//   - the embeddings table picks up the new columns
//     (embedding_id, chunk_index, chunk_char_start, chunk_char_end);
//   - legacy rows are preserved as chunk_index=0 with
//     embedding_id == legacy message_id;
//   - the vec0 table is rebuilt with embedding_id as its rowid,
//     keeping all rows so existing embeddings remain searchable;
//   - the AUTOINCREMENT counter is bumped past every legacy
//     embedding_id so new inserts don't collide with retained
//     rowids;
//   - a second Migrate is a no-op (idempotent).
func TestMigrate_LegacyToChunked(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db := openTestDB(t, path)
	t.Cleanup(func() { _ = db.Close() })

	// Hand-build the pre-chunking schema. Mirrors schema.sql as it
	// shipped at PR #277 / spec §5.2.
	legacyDDL := []string{
		`CREATE TABLE schema_version (version INTEGER PRIMARY KEY)`,
		`INSERT INTO schema_version VALUES (1)`,
		`CREATE TABLE index_generations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			model TEXT NOT NULL, dimension INTEGER NOT NULL,
			fingerprint TEXT NOT NULL, started_at INTEGER NOT NULL,
			completed_at INTEGER, activated_at INTEGER,
			state TEXT NOT NULL, message_count INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE embeddings (
			generation_id INTEGER NOT NULL REFERENCES index_generations(id) ON DELETE CASCADE,
			message_id INTEGER NOT NULL,
			embedded_at INTEGER NOT NULL,
			source_char_len INTEGER NOT NULL,
			truncated INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (generation_id, message_id)
		)`,
		`CREATE INDEX idx_embeddings_msg ON embeddings(message_id)`,
		`CREATE TABLE pending_embeddings (
			generation_id INTEGER NOT NULL REFERENCES index_generations(id) ON DELETE CASCADE,
			message_id INTEGER NOT NULL,
			enqueued_at INTEGER NOT NULL,
			claimed_at INTEGER, claim_token TEXT,
			PRIMARY KEY (generation_id, message_id)
		)`,
		`CREATE TABLE embed_runs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			generation_id INTEGER NOT NULL REFERENCES index_generations(id),
			started_at INTEGER NOT NULL, ended_at INTEGER,
			claimed INTEGER NOT NULL DEFAULT 0,
			succeeded INTEGER NOT NULL DEFAULT 0,
			failed INTEGER NOT NULL DEFAULT 0,
			truncated INTEGER NOT NULL DEFAULT 0,
			error TEXT
		)`,
		`CREATE VIRTUAL TABLE vectors_vec_d768 USING vec0(
			generation_id INTEGER PARTITION KEY,
			message_id    INTEGER PRIMARY KEY,
			embedding     FLOAT[768]
		)`,
	}
	for _, q := range legacyDDL {
		_, err := db.ExecContext(ctx, q)
		require.NoErrorf(err, "seed legacy DDL %q", q)
	}
	// Seed one generation and two embedded rows. message_count =
	// 2 to mirror what the pre-chunking Upsert path would have left
	// behind.
	_, err := db.ExecContext(ctx,
		`INSERT INTO index_generations (id, model, dimension, fingerprint, started_at, state, message_count)
		 VALUES (1, 'm', 768, 'm:768', 100, 'active', 2)`)
	require.NoError(err, "seed generation")
	_, err = db.ExecContext(ctx,
		`INSERT INTO embeddings (generation_id, message_id, embedded_at, source_char_len, truncated)
		 VALUES (1, 10, 100, 50, 0), (1, 20, 100, 75, 1)`)
	require.NoError(err, "seed embeddings")
	// vec0 demands its rowid match the second PK column; here the
	// legacy schema uses message_id, so 10 and 20 both go in directly.
	blob := func(v []float32) []byte { return float32SliceBlob(v) }
	v10 := make([]float32, 768)
	v20 := make([]float32, 768)
	for i := range v10 {
		v10[i] = 0.1
		v20[i] = 0.2
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO vectors_vec_d768 (generation_id, message_id, embedding) VALUES (1, 10, ?)`, blob(v10))
	require.NoError(err, "seed vec rowid 10")
	_, err = db.ExecContext(ctx,
		`INSERT INTO vectors_vec_d768 (generation_id, message_id, embedding) VALUES (1, 20, ?)`, blob(v20))
	require.NoError(err, "seed vec rowid 20")

	// Run the migration.
	require.NoError(Migrate(ctx, db, 768), "Migrate")

	// embeddings now has the chunked-layout columns, and the legacy
	// rows survived as chunk_index=0 with embedding_id == old message_id.
	rows, err := db.QueryContext(ctx,
		`SELECT embedding_id, message_id, chunk_index, source_char_len, truncated
		   FROM embeddings ORDER BY message_id`)
	require.NoError(err, "select embeddings")
	defer func() { _ = rows.Close() }()
	type row struct {
		eid, mid, ci, charLen, trunc int64
	}
	var got []row
	for rows.Next() {
		var r row
		require.NoError(rows.Scan(&r.eid, &r.mid, &r.ci, &r.charLen, &r.trunc), "scan")
		got = append(got, r)
	}
	// AUTOINCREMENT allocates fresh embedding_ids — they must be
	// distinct and positive. The actual values depend on insertion
	// order, so verify the *shape* (distinct, all > 0) rather than
	// pin specific numbers.
	require.Lenf(got, 2, "rows = %v", got)
	assert.Equalf(int64(10), got[0].mid, "row[0]")
	assert.Equalf(int64(0), got[0].ci, "row[0]")
	assert.Equalf(int64(50), got[0].charLen, "row[0]")
	assert.Equalf(int64(0), got[0].trunc, "row[0]")
	assert.Equalf(int64(20), got[1].mid, "row[1]")
	assert.Equalf(int64(0), got[1].ci, "row[1]")
	assert.Equalf(int64(75), got[1].charLen, "row[1]")
	assert.Equalf(int64(1), got[1].trunc, "row[1]")
	assert.Greater(got[0].eid, int64(0), "embedding_id should be positive")
	assert.Greater(got[1].eid, int64(0), "embedding_id should be positive")
	assert.NotEqual(got[0].eid, got[1].eid, "embedding_ids should be distinct")

	// vec0 rowid is now the AUTOINCREMENT embedding_id (not the
	// legacy message_id). Verify the rebuild used the mapping so
	// every legacy vec0 row joins back to its embedding.
	var n int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM vectors_vec_d768 v
		   JOIN embeddings e ON e.embedding_id = v.embedding_id
		  WHERE v.generation_id = 1`).Scan(&n)
	require.NoError(err, "join count")
	assert.Equal(2, n, "joined vec rows")

	// Idempotent: a second Migrate must do nothing and leave the rows
	// untouched.
	require.NoError(Migrate(ctx, db, 768), "second Migrate")
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM embeddings`).Scan(&n)
	require.NoError(err, "post-2nd count")
	assert.Equal(2, n, "post-2nd embeddings")
}

// TestMigrate_LegacyToChunked_MultiGenerationCollision is the
// regression test for roborev #323's high-risk finding: the legacy
// embeddings PK was (generation_id, message_id), so the same
// message_id could legitimately appear in two generations (one
// active, one building). An earlier draft of the migration mapped
// embedding_id := message_id, which collided on the new UNIQUE
// constraint as soon as that case arose. This test reproduces that
// shape and asserts the migration succeeds, allocating distinct
// embedding_ids per (gen, msg) pair and preserving every legacy
// vec0 row through the rebuild.
func TestMigrate_LegacyToChunked_MultiGenerationCollision(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db := openTestDB(t, path)
	t.Cleanup(func() { _ = db.Close() })

	legacyDDL := []string{
		`CREATE TABLE schema_version (version INTEGER PRIMARY KEY)`,
		`INSERT INTO schema_version VALUES (1)`,
		`CREATE TABLE index_generations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			model TEXT NOT NULL, dimension INTEGER NOT NULL,
			fingerprint TEXT NOT NULL, started_at INTEGER NOT NULL,
			completed_at INTEGER, activated_at INTEGER,
			state TEXT NOT NULL, message_count INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE embeddings (
			generation_id INTEGER NOT NULL REFERENCES index_generations(id) ON DELETE CASCADE,
			message_id INTEGER NOT NULL,
			embedded_at INTEGER NOT NULL,
			source_char_len INTEGER NOT NULL,
			truncated INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (generation_id, message_id)
		)`,
		`CREATE INDEX idx_embeddings_msg ON embeddings(message_id)`,
		`CREATE TABLE pending_embeddings (
			generation_id INTEGER NOT NULL REFERENCES index_generations(id) ON DELETE CASCADE,
			message_id INTEGER NOT NULL,
			enqueued_at INTEGER NOT NULL,
			claimed_at INTEGER, claim_token TEXT,
			PRIMARY KEY (generation_id, message_id)
		)`,
		`CREATE TABLE embed_runs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			generation_id INTEGER NOT NULL REFERENCES index_generations(id),
			started_at INTEGER NOT NULL, ended_at INTEGER,
			claimed INTEGER NOT NULL DEFAULT 0,
			succeeded INTEGER NOT NULL DEFAULT 0,
			failed INTEGER NOT NULL DEFAULT 0,
			truncated INTEGER NOT NULL DEFAULT 0,
			error TEXT
		)`,
		`CREATE VIRTUAL TABLE vectors_vec_d768 USING vec0(
			generation_id INTEGER PARTITION KEY,
			message_id    INTEGER PRIMARY KEY,
			embedding     FLOAT[768]
		)`,
	}
	for _, q := range legacyDDL {
		_, err := db.ExecContext(ctx, q)
		require.NoErrorf(err, "seed legacy DDL %q", q)
	}
	// Two generations: gen 1 active, gen 2 building. Both contain
	// embeddings rows for message 10 (the realistic case: an active
	// gen has it embedded; a building gen seeded it again because the
	// worker re-embeds the whole corpus per generation). The vec0
	// table only carries the gen 1 vector — vec0's rowid uniqueness
	// would have rejected the gen 2 row at write time under the
	// legacy code, so historical databases at most carry conflicting
	// embeddings rows + a single vec0 row per message_id.
	//
	// Under the buggy migration (embedding_id := message_id), the
	// gen=2/msg=10 row collided on the UNIQUE(gen,msg,ci) constraint
	// because eid=10 was already taken by gen=1/msg=10. The fixed
	// migration lets AUTOINCREMENT allocate distinct eids.
	_, err := db.ExecContext(ctx, `
		INSERT INTO index_generations (id, model, dimension, fingerprint, started_at, state, message_count) VALUES
		  (1, 'm', 768, 'm:768', 100, 'active',   1),
		  (2, 'm', 768, 'm:768', 200, 'building', 1)`)
	require.NoError(err, "seed generations")
	_, err = db.ExecContext(ctx, `
		INSERT INTO embeddings (generation_id, message_id, embedded_at, source_char_len, truncated) VALUES
		  (1, 10, 100, 50, 0),
		  (2, 10, 200, 50, 0)`)
	require.NoError(err, "seed embeddings")
	v := make([]float32, 768)
	for i := range v {
		v[i] = 0.1
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO vectors_vec_d768 (generation_id, message_id, embedding) VALUES (1, 10, ?)`,
		float32SliceBlob(v))
	require.NoError(err, "seed vec gen=1 msg=10")

	require.NoError(Migrate(ctx, db, 768), "Migrate (this is what the old migration could not survive)")

	// Both legacy embeddings rows preserved with distinct
	// embedding_ids — the AUTOINCREMENT allocation steps around the
	// (gen=1, msg=10) / (gen=2, msg=10) collision that broke the old
	// hard-coded eid=msg shortcut.
	var n int
	require.NoError(db.QueryRowContext(ctx, `SELECT COUNT(*) FROM embeddings`).Scan(&n), "count embeddings")
	assert.Equal(2, n, "embeddings rows")
	require.NoError(db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT embedding_id) FROM embeddings`).Scan(&n), "count distinct eid")
	assert.Equal(2, n, "distinct embedding_ids (one per (gen, msg))")

	// vec0 join still resolves cleanly for the row that was actually
	// embedded (gen=1, msg=10). The mapping looked up the new eid via
	// the embeddings table rather than the now-invalid eid=msg
	// shortcut.
	require.NoError(db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM vectors_vec_d768 v
		  JOIN embeddings e ON e.embedding_id = v.embedding_id
		 WHERE v.generation_id = 1 AND e.message_id = 10`).Scan(&n), "join")
	assert.Equal(1, n, "join rows")
}

func TestMigrate_CreatesDimensionSpecificVecTable(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t, filepath.Join(t.TempDir(), "v.db"))
	t.Cleanup(func() { _ = db.Close() })

	requirepkg.NoError(t, Migrate(ctx, db, 768), "migrate 768")
	requirepkg.NoError(t, EnsureVectorTable(ctx, db, 1024), "ensure 1024")
	var name string
	err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE name = 'vectors_vec_d1024'`).Scan(&name)
	assertpkg.NoError(t, err, "vectors_vec_d1024 not created")
}

func openTestDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	requirepkg.NoError(t, RegisterExtension(), "register")
	db, err := sql.Open(DriverName(), path)
	requirepkg.NoError(t, err, "open")
	return db
}

// TestForeignKeys_PerConnection verifies that `PRAGMA foreign_keys = ON`
// applies to every pooled connection, not just the one that ran Migrate.
// SQLite's foreign_keys PRAGMA is per-connection, so a single ExecContext
// against *sql.DB only enables enforcement on whatever physical conn the
// pool happened to hand back. The ConnectHook in RegisterExtension is
// what makes enforcement pool-wide.
//
// We force the pool to allocate N distinct physical connections by
// holding N *sql.Conn handles open simultaneously — sequential
// db.Conn() calls or db.ExecContext() calls can all be served by the
// same pooled conn, which would let a buggy hook hide undetected.
func TestForeignKeys_PerConnection(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "vectors.db")

	db := openTestDB(t, path)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(Migrate(ctx, db, 768), "migrate")

	const conns = 4
	db.SetMaxOpenConns(conns)

	// Acquire all N conns up front and keep them open. Each db.Conn()
	// call must allocate a fresh physical connection because earlier
	// ones haven't been released. This is what guarantees the hook is
	// being tested against distinct conns rather than a single reused
	// one.
	held := make([]*sql.Conn, conns)
	for i := 0; i < conns; i++ {
		c, err := db.Conn(ctx)
		require.NoErrorf(err, "conn %d", i)
		held[i] = c
	}
	t.Cleanup(func() {
		for _, c := range held {
			_ = c.Close()
		}
	})

	// Verify each held conn directly: PRAGMA foreign_keys must read
	// back as 1, and an FK-violating insert must fail on every conn.
	for i, c := range held {
		var fk int
		err := c.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk)
		require.NoErrorf(err, "conn %d pragma read", i)
		assert.Equalf(1, fk, "conn %d: foreign_keys (ConnectHook missed this conn)", i)
		_, err = c.ExecContext(ctx,
			`INSERT INTO pending_embeddings (generation_id, message_id, enqueued_at)
			 VALUES (?, ?, ?)`, 9999999, int64(i), int64(i))
		if !assert.Errorf(err, "conn %d: FK-violating insert should fail", i) {
			continue
		}
		msg := err.Error()
		assert.Truef(strings.Contains(msg, "FOREIGN KEY") || strings.Contains(msg, "foreign key"),
			"conn %d: error = %v; want FOREIGN KEY violation", i, err)
	}
}
