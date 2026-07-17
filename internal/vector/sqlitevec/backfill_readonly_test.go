//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

// backfillLedgerMarked reports whether the one-time upgrade backfill ledger
// row exists in the given main DB handle.
func backfillLedgerMarked(t *testing.T, db *sql.DB) bool {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM applied_migrations WHERE name = ?`,
		embedGenBackfillMigration).Scan(&n))

	return n > 0
}

// TestBackfillEmbedGen_ReadOnlyMainDB_Skipped is the regression guard for
// Codex #3: the MCP server opens the main DB query-only
// (store.OpenReadOnly, _query_only=true), but setupVectorFeatures ->
// sqlitevec.Open ran BackfillEmbedGenForUpgrade, which WRITES
// messages.embed_gen + applied_migrations through that read-only handle. The
// readOnly flag was honored on PG (SkipMigrate) but ignored on SQLite, so
// MCP startup failed (or wrote through the query-only handle) whenever the
// backfill ledger was not yet marked.
//
// With Options.ReadOnly plumbed from the MCP readOnly arg, the backfill
// self-guards: a read-only Open with an UNMARKED ledger and an active
// generation must NOT attempt the write, must NOT error, and must leave the
// ledger unmarked. Migrate still runs (vectors.db is read-write).
func TestBackfillEmbedGen_ReadOnlyMainDB_Skipped(t *testing.T) {
	assert := assert.New(
		t,
	)
	require := require.New(t)
	require.NoError(
		RegisterExtension(), "RegisterExtension")

	ctx := context.Background()

	dir := t.TempDir()
	mainPath := filepath.Join(dir, "msgvault.db")
	vecPath := filepath.Join(dir, "vectors.db")

	// Build a real main DB with one live message and an active generation
	// whose embedding exists, then reset embed_gen + clear the ledger so the
	// backfill would have real work (the embed_gen-stamping write) to do.
	s, err := store.Open(mainPath)
	require.NoError(
		err, "store.Open (rw)")

	require.NoError(
		s.InitSchema(), "InitSchema")

	_, err = s.DB().Exec(`
INSERT INTO sources (id, source_type, identifier) VALUES (1, 'gmail', 'me@example.com');
INSERT INTO conversations (id, source_id, conversation_type) VALUES (1, 1, 'email_thread');
INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type)
VALUES (1, 1, 1, 'm1', 'email');
`)
	require.NoError(
		err, "seed message")

	rw, err := Open(ctx, Options{
		Path: vecPath, MainPath: mainPath, Dimension: 4, MainDB: s.DB(),
	})
	require.NoError(
		err, "rw backend Open")

	gen, err := rw.CreateGeneration(ctx, "model", 4, "model:4")
	require.NoError(
		err, "CreateGeneration")

	require.NoError(
		rw.Upsert(ctx, gen, vectorChunkOne()), "Upsert")

	require.NoError(
		rw.ActivateGeneration(ctx, gen, true), "Activate")

	require.NoError(
		rw.Close(), "close rw backend")

	// Simulate the pre-upgrade state: embed_gen NULL everywhere, ledger
	// cleared. A WRITE-path Open here would stamp embed_gen and mark the
	// ledger; a READ-only Open must do neither.
	_, err = s.DB().Exec(`UPDATE messages SET embed_gen = NULL`)
	require.NoError(
		err, "reset embed_gen")

	_, err = s.DB().Exec(`DELETE FROM applied_migrations WHERE name = ?`, embedGenBackfillMigration)
	require.NoError(
		err, "clear ledger")

	require.NoError(
		s.Close(), "close rw store")

	// Reopen the main DB read-only, exactly as the MCP server does.
	ro, err := store.OpenReadOnly(mainPath)
	require.NoError(
		err, "store.OpenReadOnly")

	defer func() { _ = ro.Close() }()

	// MCP path: sqlitevec.Open with ReadOnly=true. The backfill must be
	// skipped — no write through the query-only handle, no error.
	b, err := Open(ctx, Options{
		Path:      vecPath,
		MainPath:  mainPath,
		Dimension: 4,
		MainDB:    ro.DB(),
		ReadOnly:  true,
	})
	require.NoError(
		err, "read-only Open must not error (backfill skipped)")

	defer func() { _ = b.Close() }()
	assert. // Verify nothing was written: ledger stays unmarked, embed_gen stays NULL.
		False(backfillLedgerMarked(t, ro.DB()),
			"read-only Open must NOT mark the backfill ledger")
	var v sql.NullInt64
	require.NoError(ro.DB().QueryRow(`SELECT embed_gen FROM messages WHERE id = 1`).Scan(&v))
	assert.False(v.Valid, "read-only Open must NOT stamp embed_gen")
}

// vectorChunkOne returns a single-chunk slice for message 1 with a unit
// 4-dim vector — the embedding the read-only backfill test pre-seeds.
func vectorChunkOne() []vector.Chunk {
	return []vector.Chunk{{MessageID: 1, ChunkIndex: 0, Vector: []float32{0, 0, 0, 1}}}
}

// TestResetOrphanedEmbedGen_ReadOnlyMainDB_Skipped is the read-only guard for
// the orphaned-stamp reset (Codex 129c #1). The reset WRITES
// messages.embed_gen, so a read-only main handle (MCP: store.OpenReadOnly,
// _query_only=true) must SKIP it entirely — no write attempt, no error, stamps
// untouched. Mirrors the backfill's b.readOnly guard.
//
// The setup leaves an ORPHANED stamp (embed_gen=99 with an empty
// index_generations) so a writable Open WOULD reset it; the read-only Open
// must not.
func TestResetOrphanedEmbedGen_ReadOnlyMainDB_Skipped(t *testing.T) {
	assert := assert.New(
		t,
	)
	require := require.New(t)
	require.NoError(
		RegisterExtension(), "RegisterExtension")

	ctx := context.Background()

	dir := t.TempDir()
	mainPath := filepath.Join(dir, "msgvault.db")
	vecPath := filepath.Join(dir, "vectors.db")

	// Build a real main DB with one live message whose embed_gen references a
	// generation id that does NOT exist in the (empty) vectors.db
	// index_generations — i.e. an orphaned stamp.
	s, err := store.Open(mainPath)
	require.NoError(
		err, "store.Open (rw)")

	require.NoError(
		s.InitSchema(), "InitSchema")

	_, err = s.DB().Exec(`
INSERT INTO sources (id, source_type, identifier) VALUES (1, 'gmail', 'me@example.com');
INSERT INTO conversations (id, source_id, conversation_type) VALUES (1, 1, 'email_thread');
INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, embed_gen)
VALUES (1, 1, 1, 'm1', 'email', 99);
`)
	require.NoError(
		err, "seed message with orphaned embed_gen")

	require.NoError(
		s.Close(), "close rw store")

	// Reopen the main DB read-only, exactly as the MCP server does. Migrate
	// will create an empty index_generations in vectors.db (read-write), so id
	// 99 is orphaned; the reset would clear it on a WRITABLE open.
	ro, err := store.OpenReadOnly(mainPath)
	require.NoError(
		err, "store.OpenReadOnly")

	defer func() { _ = ro.Close() }()

	b, err := Open(ctx, Options{
		Path:      vecPath,
		MainPath:  mainPath,
		Dimension: 4,
		MainDB:    ro.DB(),
		ReadOnly:  true,
	})
	require.NoError(
		err, "read-only Open must not error (reset skipped)")

	defer func() { _ = b.Close() }()

	// The orphaned stamp must be PRESERVED: a read-only Open writes nothing.
	var v sql.NullInt64
	require.NoError(ro.DB().QueryRow(`SELECT embed_gen FROM messages WHERE id = 1`).Scan(&v))
	assert.True(v.Valid, "read-only Open must NOT reset the orphaned embed_gen")
	assert.Equal(int64(99), v.Int64, "orphaned stamp unchanged under read-only Open")
}
