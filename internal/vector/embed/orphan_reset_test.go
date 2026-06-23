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

// orphanFixture stands up a main DB (messages + bodies + applied_migrations)
// and a vectors.db at known paths, so a test can stamp orphaned embed_gen
// values and then RE-OPEN the backend to exercise resetOrphanedEmbedGen
// (which only runs from sqlitevec.Open).
type orphanFixture struct {
	MainDB   *sql.DB
	MainPath string
	VecPath  string
	Store    WorkStore
	Client   *fakeEmbeddingClient
}

// newOrphanFixture creates n live messages (id 1..n) with NULL embed_gen,
// plus the message_bodies and applied_migrations tables, at known file paths.
// It does NOT open a backend; the caller opens (and re-opens) as needed.
func newOrphanFixture(t *testing.T, n int) *orphanFixture {
	t.Helper()

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

	return &orphanFixture{
		MainDB:   mainDB,
		MainPath: mainPath,
		VecPath:  filepath.Join(dir, "vectors.db"),
		Store:    &testWorkStore{db: mainDB},
		Client:   &fakeEmbeddingClient{dim: 4},
	}
}

// openBackend opens (or re-opens) the sqlitevec backend over the fixture's
// vectors.db + main DB, registering cleanup.
func (f *orphanFixture) openBackend(ctx context.Context, t *testing.T) *sqlitevec.Backend {
	t.Helper()
	b, err := sqlitevec.Open(ctx, sqlitevec.Options{
		Path:      f.VecPath,
		MainPath:  f.MainPath,
		Dimension: 4,
		MainDB:    f.MainDB,
	})
	require.NoError(t, err, "sqlitevec.Open")
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// TestResetOrphanedEmbedGen_RecreateScenario reproduces the vectors.db-
// recreate bug (Codex 129c #1): main.db carries embed_gen=1 stamps but the
// (fresh / empty) index_generations does NOT contain id 1. Opening the backend
// writable must reset those orphaned stamps to NULL BEFORE any rebuild can
// reuse id 1, so coverage reports them missing and a subsequent build re-embeds
// them — no false "done"/empty-index activation.
func TestResetOrphanedEmbedGen_RecreateScenario(t *testing.T) {
	ctx := context.Background()
	f := newOrphanFixture(t, 2)

	// Simulate a recreated vectors.db: empty index_generations, but main.db
	// already stamps both messages embed_gen=1 (the old, now-gone gen id).
	_, err := f.MainDB.ExecContext(ctx, `UPDATE messages SET embed_gen = 1`)
	require.NoError(t, err, "stamp orphaned embed_gen=1")

	// Sanity: WITHOUT the reset, coverage for a freshly-created gen id 1 would
	// (wrongly) read these as covered. Confirm both are currently stamped.
	require.Equal(t, 0, countMissing(t, f.MainDB, 1), "pre-open: stamps mask coverage")

	// Open writable: index_generations is empty, so the valid-id set is empty
	// and ALL non-NULL stamps are orphaned -> cleared.
	b := f.openBackend(ctx, t)

	for _, id := range []int64{1, 2} {
		_, isNull := embedGenOf(t, f.MainDB, id)
		assert.Truef(t, isNull, "msg %d embed_gen reset to NULL after recreate open", id)
	}

	// Now a rebuild creates a fresh gen (id 1, reusing the AUTOINCREMENT seed)
	// and coverage correctly reports both messages missing.
	gen, err := b.CreateGeneration(ctx, "fake", 4, "")
	require.NoError(t, err, "CreateGeneration")
	require.Equal(t, int64(1), int64(gen), "fresh vectors.db restarts gen ids at 1")
	assert.Equal(t, 2, countMissing(t, f.MainDB, int64(gen)),
		"both messages missing for the reused gen id (no false coverage)")

	// A worker RunOnce re-embeds both, so the index is NOT empty.
	w := NewWorker(WorkerDeps{
		Backend:   b,
		VectorsDB: mustOpenVecDB(t, f.VecPath),
		MainDB:    f.MainDB,
		Store:     f.Store,
		Client:    f.Client,
		BatchSize: 8,
	})
	res, err := w.RunOnce(ctx, gen)
	require.NoError(t, err, "RunOnce")
	assert.Equal(t, 2, res.Succeeded, "both messages re-embedded after reset")
	assert.Equal(t, 0, countMissing(t, f.MainDB, int64(gen)), "coverage complete after re-embed")
}

// TestResetOrphanedEmbedGen_NoFalsePositive verifies the reset PRESERVES
// stamps that reference a still-existing generation row. A message stamped for
// a real, retained gen (active or retired — retire only flips state) must NOT
// be reset, so the normal activate/retire flow never re-embeds good data.
func TestResetOrphanedEmbedGen_NoFalsePositive(t *testing.T) {
	ctx := context.Background()
	f := newOrphanFixture(t, 2)

	// First Open: empty vectors.db. Create + activate a real generation with
	// both messages embedded and stamped — the normal "fully covered" state.
	b := f.openBackend(ctx, t)
	gen, err := b.CreateGeneration(ctx, "fake", 4, "")
	require.NoError(t, err, "CreateGeneration")
	require.NoError(t, b.Upsert(ctx, gen, []vector.Chunk{
		{MessageID: 1, Vector: []float32{1, 0, 0, 0}},
		{MessageID: 2, Vector: []float32{0, 1, 0, 0}},
	}), "Upsert")
	require.NoError(t, f.Store.SetEmbedGen(ctx, []int64{1, 2}, int64(gen)), "stamp")
	require.NoError(t, b.ActivateGeneration(ctx, gen, true), "Activate")
	require.NoError(t, b.Close(), "close first backend")

	// Re-open writable: gen still exists in index_generations, so its stamps
	// must be PRESERVED (not reset).
	f.openBackend(ctx, t)

	for _, id := range []int64{1, 2} {
		v, isNull := embedGenOf(t, f.MainDB, id)
		assert.Falsef(t, isNull, "msg %d stamp preserved (gen still exists)", id)
		assert.Equalf(t, int64(gen), v, "msg %d embed_gen preserved", id)
	}
	assert.Equal(t, 0, countMissing(t, f.MainDB, int64(gen)),
		"coverage stays complete; no spurious re-embed")
}

// mustOpenVecDB opens a second handle to vectors.db for the worker's
// embed_runs / watermark writes, mirroring newBackfillFixture.
func mustOpenVecDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open(sqlitevec.DriverName(), path)
	require.NoError(t, err, "open vectors.db handle")
	t.Cleanup(func() { _ = db.Close() })
	return db
}
