//go:build sqlite_vec

package embed

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

// backfillFixture stands up a real sqlitevec backend over a main DB that
// includes an applied_migrations ledger (which newWorkerFixture omits), so
// the one-time embed_gen upgrade backfill can be exercised
// end-to-end against the worker.
type backfillFixture struct {
	MainDB    *sql.DB
	VectorsDB *sql.DB
	Backend   *sqlitevec.Backend
	Store     WorkStore
	Client    *fakeEmbeddingClient
}

// newBackfillFixture creates n messages (id 1..n) with NULL embed_gen plus
// the applied_migrations ledger and message_bodies, and opens a backend.
func newBackfillFixture(t *testing.T, n int) *backfillFixture {
	t.Helper()
	ctx := context.Background()

	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.db")
	require.NoError(t, sqlitevec.RegisterExtension(), "RegisterExtension")
	mainDB, err := sql.Open(sqlitevec.DriverName(), mainPath)
	require.NoError(t, err, "open main")
	t.Cleanup(func() { _ = mainDB.Close() })

	schema := testMainSchema + `
CREATE TABLE applied_migrations (
    name TEXT PRIMARY KEY,
    applied_at DATETIME DEFAULT CURRENT_TIMESTAMP
);`
	_, err = mainDB.Exec(schema)
	require.NoError(t, err, "schema")
	for i := 1; i <= n; i++ {
		_, err := mainDB.Exec(
			`INSERT INTO messages (id, subject) VALUES (?, ?)`, i, fmt.Sprintf("msg %d", i))
		require.NoError(t, err, "insert message")
		_, err = mainDB.Exec(
			`INSERT INTO message_bodies (message_id, body_text) VALUES (?, ?)`, i, fmt.Sprintf("body %d", i))
		require.NoError(t, err, "insert body")
	}

	vecPath := filepath.Join(dir, "vectors.db")
	b, err := sqlitevec.Open(ctx, sqlitevec.Options{
		Path:      vecPath,
		MainPath:  mainPath,
		Dimension: 4,
		MainDB:    mainDB,
	})
	require.NoError(t, err, "sqlitevec.Open")
	t.Cleanup(func() { _ = b.Close() })

	vecDB, err := sql.Open(sqlitevec.DriverName(), vecPath)
	require.NoError(t, err, "open vectors.db handle")
	t.Cleanup(func() { _ = vecDB.Close() })

	return &backfillFixture{
		MainDB:    mainDB,
		VectorsDB: vecDB,
		Backend:   b,
		Store:     &testWorkStore{db: mainDB},
		Client:    &fakeEmbeddingClient{dim: 4},
	}
}

func embedGenOf(t *testing.T, db *sql.DB, id int64) (val int64, isNull bool) {
	t.Helper()
	var v sql.NullInt64
	require.NoError(t, db.QueryRow(`SELECT embed_gen FROM messages WHERE id = ?`, id).Scan(&v))
	return v.Int64, !v.Valid
}

// TestBackfillEmbedGen_UpgradeStampsEmbeddedOnly simulates an upgrade from
// a pre-embed_gen build: an active generation already has embeddings for
// some messages, but embed_gen is NULL everywhere (the ADD COLUMN did no
// backfill). The one-time backfill must stamp embed_gen=active for the
// already-embedded messages and leave the un-embedded one NULL; coverage
// then becomes honest; re-running the backfill is a ledger-guarded no-op;
// and a worker RunOnce re-embeds ONLY the un-embedded straggler.
func TestBackfillEmbedGen_UpgradeStampsEmbeddedOnly(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	// 3 messages: 1 and 2 will be embedded under the active gen; 3 will not.
	f := newBackfillFixture(t, 3)

	gen, err := f.Backend.CreateGeneration(ctx, "fake", 4, "")
	require.NoError(
		err, "CreateGeneration")

	// Embed messages 1 and 2 under the generation (upsert vectors). This is
	// the "already embedded before upgrade" state.
	chunks := []vector.Chunk{
		{MessageID: 1, Vector: []float32{1, 0, 0, 0}},
		{MessageID: 2, Vector: []float32{0, 1, 0, 0}},
	}
	require.NoError(
		f.Backend.Upsert(ctx, gen, chunks), "Upsert")

	require.NoError(

		f.Store.SetEmbedGen(ctx, []int64{1, 2, 3}, int64(gen)), "stamp")

	require.NoError(
		f.Backend.ActivateGeneration(ctx, gen, true), "activate (force)")

	_, err = f.MainDB.ExecContext(ctx, `UPDATE messages SET embed_gen = NULL`)
	require.NoError(
		err, "reset embed_gen to NULL (simulate upgrade)")

	require. // Sanity: coverage now (wrongly) reports all 3 as missing.
			Equal(3, countMissing(t, f.MainDB, int64(gen)), "pre-backfill: all missing")

	// newBackfillFixture's Open already ran (and marked) the backfill when
	// no generation existed. Clear the ledger row so the manual call below
	// reproduces the real upgrade timing: the first Open where an active
	// generation + pre-existing embeddings are present.
	_, err = f.MainDB.ExecContext(ctx,
		`DELETE FROM applied_migrations WHERE name = ?`, "embed_gen_backfill_active_v1")
	require.NoError(
		err, "reset ledger")

	require.NoError(

		f.Backend.BackfillEmbedGenForUpgrade(ctx), "backfill")

	// Messages 1 and 2 (already embedded) are stamped; 3 stays NULL.
	for _, id := range []int64{1, 2} {
		v, isNull := embedGenOf(t, f.MainDB, id)
		assert.Falsef(isNull, "msg %d should be stamped", id)
		assert.Equalf(int64(gen), v, "msg %d embed_gen", id)
	}
	v3, isNull3 := embedGenOf(t, f.MainDB, 3)
	assert.True(isNull3, "msg 3 (un-embedded) stays NULL")
	_ = v3
	assert. // Coverage is now honest: only message 3 is missing.
		Equal(1, countMissing(t, f.MainDB, int64(gen)), "post-backfill: only msg 3 missing")
	require.NoError(

		f.Backend.BackfillEmbedGenForUpgrade(ctx), "backfill again (no-op)")

	_, isNull3Again := embedGenOf(t, f.MainDB, 3)
	assert.True(isNull3Again, "msg 3 still NULL after second backfill (ledger no-op)")

	// A worker RunOnce against the active generation must re-embed ONLY the
	// straggler (message 3), not the already-stamped 1 and 2.
	w := NewWorker(WorkerDeps{
		Backend:   f.Backend,
		VectorsDB: f.VectorsDB,
		MainDB:    f.MainDB,
		Store:     f.Store,
		Client:    f.Client,
		BatchSize: 8,
	})
	res, err := w.RunOnce(ctx, gen)
	require.NoError(
		err, "RunOnce")

	assert.Equal(1, res.Succeeded, "worker re-embeds only the un-stamped straggler")
	assert.Equal(0, countMissing(t, f.MainDB, int64(gen)), "coverage complete after straggler embedded")
}

// TestBackfillEmbedGen_NoActiveGenerationMarksLedger verifies the backfill
// no-ops cleanly (and marks the ledger) when there is no active generation:
// nothing to stamp, but the migration is recorded so it never re-runs.
func TestBackfillEmbedGen_NoActiveGenerationMarksLedger(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	f := newBackfillFixture(t, 2)

	// newBackfillFixture's Open already marked the ledger (no gen at open
	// time); clear it so this call is the one that marks it.
	_, err := f.MainDB.ExecContext(ctx,
		`DELETE FROM applied_migrations WHERE name = ?`, "embed_gen_backfill_active_v1")
	require.NoError(
		err, "reset ledger")

	require.NoError(
		f.Backend.BackfillEmbedGenForUpgrade(ctx), "backfill (no active gen)")

	var n int
	require.NoError(f.MainDB.QueryRow(
		`SELECT COUNT(*) FROM applied_migrations WHERE name = ?`,
		"embed_gen_backfill_active_v1").Scan(&n))
	assert.Equal(1, n, "ledger marked even with no active generation")

	// Both messages remain NULL (no embeddings to stamp from).
	for _, id := range []int64{1, 2} {
		_, isNull := embedGenOf(t, f.MainDB, id)
		assert.Truef(isNull, "msg %d stays NULL", id)
	}
}
