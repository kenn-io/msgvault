//go:build pgvector

package embed

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
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/pgvector"
)

// openPGEnqueueDB stands up a per-test schema on MSGVAULT_TEST_DB with the
// pgvector schema (index_generations + pending_embeddings). The Enqueuer
// only reads index_generations and writes pending_embeddings, so the main
// messages table is not needed here. Returns the *sql.DB; cleanup drops
// the schema via t.Cleanup.
func openPGEnqueueDB(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(url, "postgres://") && !strings.HasPrefix(url, "postgresql://") {
		t.Skip("pgvector enqueue tests require MSGVAULT_TEST_DB to point at a PostgreSQL DSN")
	}

	buf := make([]byte, 8)
	_, err := rand.Read(buf)
	require.NoError(t, err, "random schema name")
	schemaName := "embed_e_test_" + hex.EncodeToString(buf)

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

	require.NoError(t, pgvector.Migrate(context.Background(), db, 0, false), "pgvector.Migrate")
	return db
}

// insertPGGeneration inserts an index_generations row with an explicit id
// and state so the test can control which generations are non-retired.
func insertPGGeneration(t *testing.T, db *sql.DB, id int64, state string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO index_generations (id, model, dimension, fingerprint, started_at, state)
		OVERRIDING SYSTEM VALUE
		VALUES ($1, 'm', 768, 'm:768', 0, $2)`, id, state)
	require.NoError(t, err, "insert generation")
}

func pgPendingCount(t *testing.T, db *sql.DB, gen int64) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = $1`, gen).Scan(&n),
		"pending count")
	return n
}

func pgEnqueuer(db *sql.DB) *Enqueuer {
	d := &store.PostgreSQLDialect{}
	return NewEnqueuer(db, d.Rebind, d.InsertOrIgnore)
}

// TestEnqueuerPG_DualEnqueueAndRetiredExclusion asserts that on pgx the
// Enqueuer inserts one pending row per (non-retired generation, message)
// and skips retired generations. Before the json_each → chunked-VALUES
// port this failed against pgx: json_each is SQLite-only and the bare `?`
// placeholders are rejected by the pgx driver.
func TestEnqueuerPG_DualEnqueueAndRetiredExclusion(t *testing.T) {
	ctx := context.Background()
	db := openPGEnqueueDB(t)
	insertPGGeneration(t, db, 1, "active")
	insertPGGeneration(t, db, 2, "building")
	insertPGGeneration(t, db, 3, "retired") // must NOT receive rows.

	e := pgEnqueuer(db)
	require.NoError(t, e.EnqueueMessages(ctx, []int64{10, 11, 12}), "EnqueueMessages")

	assert.Equal(t, 3, pgPendingCount(t, db, 1), "active generation pending count")
	assert.Equal(t, 3, pgPendingCount(t, db, 2), "building generation pending count")
	assert.Equal(t, 0, pgPendingCount(t, db, 3), "retired generation must be excluded")
}

// TestEnqueuerPG_RetireDuringEnqueue_NoOrphan drives the concurrent
// retire-during-enqueue interleaving that the locked per-generation
// re-validation closes. It forces the exact window the orphan-pending race
// opens — the enqueue tx reads the non-retired snapshot, THEN a concurrent
// RetireGeneration commits (UPDATE state='retired' + DELETE pending), THEN
// the enqueue attempts its inserts — and asserts no pending row is left
// behind for the now-retired generation.
//
// Without the fix the enqueue inserts pending rows for the snapshotted
// (now-retired) generation after retire's DELETE has run, so an orphan row
// commits and is never reaped. With the fix the locked re-read sees
// state='retired' and skips the generation, so the post-state has zero
// pending rows for it.
//
// The interleave is made deterministic via afterGenSnapshotHook: the hook
// fires inside the enqueue tx after the snapshot read and runs the retire
// to completion before returning, so the enqueue's re-validation always
// observes the committed retire.
func TestEnqueuerPG_RetireDuringEnqueue_NoOrphan(t *testing.T) {
	ctx := context.Background()
	db := openPGEnqueueDB(t)

	// Gen 1 stays active (so a non-force retire of gen 2 is permitted) and
	// is the control that must keep its rows. Gen 2 is the building gen that
	// gets retired mid-enqueue and must end with zero pending rows.
	insertPGGeneration(t, db, 1, "active")
	insertPGGeneration(t, db, 2, "building")

	backend, err := pgvector.Open(ctx, pgvector.Options{DB: db, SkipMigrate: true})
	require.NoError(t, err, "open pgvector backend")

	// The hook fires once, inside the enqueue tx, after the non-retired
	// snapshot (which still includes gen 2) is read. We retire gen 2 to
	// completion here so the enqueue's subsequent locked re-validation
	// observes the committed state='retired'. Reset the seam so it cannot
	// leak into sibling tests sharing this package's globals.
	var retireErr error
	afterGenSnapshotHook = func() {
		retireErr = backend.RetireGeneration(ctx, 2, false)
	}
	t.Cleanup(func() { afterGenSnapshotHook = nil })

	e := pgEnqueuer(db)
	require.NoError(t, e.EnqueueMessages(ctx, []int64{10, 11, 12}), "EnqueueMessages")
	require.NoError(t, retireErr, "RetireGeneration during enqueue")

	// Gen 2 was retired before the enqueue inserted its rows: the locked
	// re-validation must have excluded it, leaving zero orphan pending rows.
	assert.Equal(t, 0, pgPendingCount(t, db, 2),
		"retired-mid-enqueue generation must have no orphan pending rows")
	// Gen 1 stayed active throughout, so it still receives every id.
	assert.Equal(t, 3, pgPendingCount(t, db, 1),
		"active generation still enqueued despite concurrent retire")

	// Sanity: gen 2 really is retired.
	var state string
	require.NoError(t, db.QueryRow(
		`SELECT state FROM index_generations WHERE id = $1`, int64(2)).Scan(&state))
	assert.Equal(t, string(vector.GenerationRetired), state, "gen 2 retired")
}

// TestEnqueuerPG_Idempotent asserts re-enqueueing the same IDs is a no-op
// via ON CONFLICT (generation_id, message_id) DO NOTHING — exercised both
// across calls and within a single call carrying duplicate IDs.
func TestEnqueuerPG_Idempotent(t *testing.T) {
	ctx := context.Background()
	db := openPGEnqueueDB(t)
	insertPGGeneration(t, db, 1, "active")

	e := pgEnqueuer(db)
	require.NoError(t, e.EnqueueMessages(ctx, []int64{42}), "first enqueue")
	// Re-enqueue across calls and with an intra-call duplicate.
	require.NoError(t, e.EnqueueMessages(ctx, []int64{42, 42}), "re-enqueue with duplicates")
	assert.Equal(t, 1, pgPendingCount(t, db, 1), "duplicate (gen, message) must collapse to one row")
}

// TestEnqueuerPG_MultiChunk enqueues more IDs than enqueueChunkRows so the
// chunked-VALUES insert spans more than one statement. This exercises the
// chunk loop's boundary handling and confirms the total parameter count
// per statement stays bounded on pgx. The count uses
// 2*enqueueChunkRows + 1 so the final chunk is a small remainder.
func TestEnqueuerPG_MultiChunk(t *testing.T) {
	ctx := context.Background()
	db := openPGEnqueueDB(t)
	insertPGGeneration(t, db, 1, "active")
	insertPGGeneration(t, db, 2, "building")

	const total = 2*enqueueChunkRows + 1
	ids := make([]int64, total)
	for i := range ids {
		ids[i] = int64(i + 1)
	}

	e := pgEnqueuer(db)
	require.NoError(t, e.EnqueueMessages(ctx, ids), "EnqueueMessages spanning multiple chunks")

	assert.Equal(t, total, pgPendingCount(t, db, 1), "active generation got every id across chunks")
	assert.Equal(t, total, pgPendingCount(t, db, 2), "building generation got every id across chunks")

	// Re-enqueue the full batch: still idempotent across multiple chunks.
	require.NoError(t, e.EnqueueMessages(ctx, ids), "re-enqueue multi-chunk batch")
	assert.Equal(t, total, pgPendingCount(t, db, 1), "active count unchanged after idempotent re-enqueue")
}

// TestEnqueuerPG_NoGenerations_Noop asserts EnqueueMessages is a clean
// no-op when there are no non-retired generations.
func TestEnqueuerPG_NoGenerations_Noop(t *testing.T) {
	ctx := context.Background()
	db := openPGEnqueueDB(t)
	insertPGGeneration(t, db, 1, "retired")

	e := pgEnqueuer(db)
	require.NoError(t, e.EnqueueMessages(ctx, []int64{1, 2, 3}), "EnqueueMessages with only retired gen")

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM pending_embeddings`).Scan(&n))
	assert.Equal(t, 0, n, "no pending rows when only retired generations exist")
}
