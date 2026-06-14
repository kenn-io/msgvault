//go:build pgvector

package embed

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector/pgvector"
)

// newPGQueueSchema stands up a per-test schema on MSGVAULT_TEST_DB, applies
// the pgvector schema, and seeds one building generation (id=1) with n
// pending rows. It returns BOTH the *sql.DB and the schema-scoped DSN so
// callers that need a SECOND independent handle on the SAME schema (e.g. the
// SKIP-LOCKED concurrency test) can sql.Open the returned dsn. The schema is
// dropped on cleanup. Skips when MSGVAULT_TEST_DB is not a PostgreSQL DSN.
func newPGQueueSchema(t *testing.T, n int) (db *sql.DB, dsn string) {
	t.Helper()
	url := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(url, "postgres://") && !strings.HasPrefix(url, "postgresql://") {
		t.Skip("pgvector queue tests require MSGVAULT_TEST_DB to point at a PostgreSQL DSN")
	}

	buf := make([]byte, 8)
	_, err := rand.Read(buf)
	require.NoError(t, err, "random schema name")
	schemaName := "embed_q_test_" + hex.EncodeToString(buf)

	setup, err := sql.Open("pgx", url)
	require.NoError(t, err, "open setup")
	defer func() { _ = setup.Close() }()
	_, err = setup.Exec("CREATE SCHEMA " + schemaName)
	require.NoError(t, err, "create schema")

	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	dsn = url + sep + "search_path=" + schemaName + ",public"

	db, err = sql.Open("pgx", dsn)
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

	ctx := context.Background()
	require.NoError(t, pgvector.Migrate(ctx, db, 0, false), "pgvector.Migrate")

	_, err = db.ExecContext(ctx, `
		INSERT INTO index_generations (id, model, dimension, fingerprint, started_at, state)
		OVERRIDING SYSTEM VALUE
		VALUES (1, 'm', 768, 'm:768', 0, 'building')`)
	require.NoError(t, err, "insert generation")
	for i := 1; i <= n; i++ {
		_, err := db.ExecContext(ctx,
			`INSERT INTO pending_embeddings (generation_id, message_id, enqueued_at) VALUES (1, $1, 0)`,
			i)
		require.NoError(t, err, "insert pending")
	}
	return db, dsn
}

// openPGQueueDB stands up a per-test schema seeded with n pending rows and
// returns the *sql.DB. Thin wrapper over newPGQueueSchema for the common
// single-handle case; cleanup drops the schema via t.Cleanup.
func openPGQueueDB(t *testing.T, n int) *sql.DB {
	t.Helper()
	db, _ := newPGQueueSchema(t, n)
	return db
}

// pgCountAvailable returns the number of available (unclaimed) rows for
// the single building generation (id=1) that newPGQueueSchema seeds —
// the only generation these queue tests create.
func pgCountAvailable(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = 1 AND claimed_at IS NULL`).Scan(&n)
	require.NoError(t, err, "countAvailable")
	return n
}

func pgRebind() func(string) string {
	return (&store.PostgreSQLDialect{}).Rebind
}

func TestQueuePG_ClaimReleaseComplete(t *testing.T) {
	ctx := context.Background()
	db := openPGQueueDB(t, 5)
	q := NewQueue(db, pgRebind())

	ids, token, err := q.Claim(ctx, 1, 3)
	require.NoError(t, err, "Claim")
	require.Len(t, ids, 3)
	require.NotEmpty(t, token)

	more, token2, err := q.Claim(ctx, 1, 10)
	require.NoError(t, err)
	assert.Len(t, more, 2)
	assert.NotEqual(t, token, token2, "second claim must use a fresh token")

	require.NoError(t, q.Release(ctx, 1, token, ids), "Release")
	assert.Equal(t, 3, pgCountAvailable(t, db), "available after release")

	require.NoError(t, q.Complete(ctx, 1, token2, more), "Complete")
	var total int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM pending_embeddings`).Scan(&total))
	assert.Equal(t, 3, total, "pending total after complete = 5 - 2")
}

func TestQueuePG_Claim_EmptyBatchIsNoop(t *testing.T) {
	ctx := context.Background()
	db := openPGQueueDB(t, 1)
	q := NewQueue(db, pgRebind())
	ids, token, err := q.Claim(ctx, 1, 0)
	require.NoError(t, err, "Claim(0)")
	assert.Empty(t, ids)
	assert.Empty(t, token)
}

func TestQueuePG_Claim_NoAvailableReturnsEmpty(t *testing.T) {
	ctx := context.Background()
	db := openPGQueueDB(t, 0)
	q := NewQueue(db, pgRebind())
	ids, token, err := q.Claim(ctx, 1, 10)
	require.NoError(t, err, "Claim")
	assert.Empty(t, ids)
	assert.Empty(t, token)
}

func TestQueuePG_Complete_WrongTokenNoop(t *testing.T) {
	ctx := context.Background()
	db := openPGQueueDB(t, 2)
	q := NewQueue(db, pgRebind())
	ids, _, err := q.Claim(ctx, 1, 2)
	require.NoError(t, err)
	require.NoError(t, q.Complete(ctx, 1, "deadbeef", ids), "Complete with wrong token")
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM pending_embeddings`).Scan(&n))
	assert.Equal(t, 2, n, "Complete should not delete on token mismatch")
}

func TestQueuePG_Release_WrongTokenNoop(t *testing.T) {
	ctx := context.Background()
	db := openPGQueueDB(t, 2)
	q := NewQueue(db, pgRebind())
	ids, _, err := q.Claim(ctx, 1, 2)
	require.NoError(t, err)
	require.NoError(t, q.Release(ctx, 1, "deadbeef", ids), "Release with wrong token")
	assert.Equal(t, 0, pgCountAvailable(t, db), "available after wrong-token release (still claimed)")
}

func TestQueuePG_ReclaimStale(t *testing.T) {
	ctx := context.Background()
	db := openPGQueueDB(t, 2)
	q := NewQueue(db, pgRebind())
	_, _, err := q.Claim(ctx, 1, 2)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`UPDATE pending_embeddings SET claimed_at = $1 WHERE generation_id = 1`,
		time.Now().Add(-20*time.Minute).Unix())
	require.NoError(t, err)
	n, err := q.ReclaimStale(ctx, 10*time.Minute)
	require.NoError(t, err)
	assert.Equal(t, 2, n, "reclaimed")
	assert.Equal(t, 2, pgCountAvailable(t, db), "available after reclaim")
}

func TestQueuePG_Complete_EmptyIDsIsNoop(t *testing.T) {
	ctx := context.Background()
	db := openPGQueueDB(t, 1)
	q := NewQueue(db, pgRebind())
	assert.NoError(t, q.Complete(ctx, 1, "token", nil), "Complete(nil)")
}

func TestQueuePG_Claim_ReturnsIDsAscending(t *testing.T) {
	ctx := context.Background()
	db := openPGQueueDB(t, 10)
	q := NewQueue(db, pgRebind())

	ids, _, err := q.Claim(ctx, 1, 10)
	require.NoError(t, err, "Claim")
	require.Len(t, ids, 10)
	assert.True(t, sort.SliceIsSorted(ids, func(i, j int) bool { return ids[i] < ids[j] }),
		"ids not ascending: %v", ids)
}

// TestQueuePG_CompleteRelease_ChunksLargeIDSets is the PG counterpart of
// TestQueue_CompleteRelease_ChunksLargeIDSets: it lowers the chunk size
// so Complete/Release span multiple token-scoped statements on pgx, then
// asserts every intended row is released/deleted.
func TestQueuePG_CompleteRelease_ChunksLargeIDSets(t *testing.T) {
	ctx := context.Background()

	orig := completeReleaseChunkRows
	completeReleaseChunkRows = 2
	t.Cleanup(func() { completeReleaseChunkRows = orig })

	const n = 5
	db := openPGQueueDB(t, n)
	q := NewQueue(db, pgRebind())

	ids, token, err := q.Claim(ctx, 1, n)
	require.NoError(t, err, "Claim")
	require.Len(t, ids, n)

	require.NoError(t, q.Release(ctx, 1, token, ids), "Release (chunked)")
	assert.Equal(t, n, pgCountAvailable(t, db), "all rows available after chunked Release")

	ids2, token2, err := q.Claim(ctx, 1, n)
	require.NoError(t, err, "re-Claim")
	require.Len(t, ids2, n)
	require.NoError(t, q.Complete(ctx, 1, token2, ids2), "Complete (chunked)")
	var total int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM pending_embeddings`).Scan(&total))
	assert.Equal(t, 0, total, "all rows deleted after chunked Complete")
}

// TestQueuePG_CompleteRelease_ChunkedTokenScoped verifies the chunked
// pgx path preserves the token filter across chunk boundaries.
func TestQueuePG_CompleteRelease_ChunkedTokenScoped(t *testing.T) {
	ctx := context.Background()

	orig := completeReleaseChunkRows
	completeReleaseChunkRows = 2
	t.Cleanup(func() { completeReleaseChunkRows = orig })

	const n = 5
	db := openPGQueueDB(t, n)
	q := NewQueue(db, pgRebind())

	ids, _, err := q.Claim(ctx, 1, n)
	require.NoError(t, err, "Claim")
	require.Len(t, ids, n)

	require.NoError(t, q.Complete(ctx, 1, "deadbeef", ids), "Complete wrong token (chunked)")
	var total int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM pending_embeddings`).Scan(&total))
	assert.Equal(t, n, total, "wrong-token chunked Complete must not delete")

	require.NoError(t, q.Release(ctx, 1, "deadbeef", ids), "Release wrong token (chunked)")
	assert.Equal(t, 0, pgCountAvailable(t, db), "wrong-token chunked Release must leave rows claimed")
}

// TestQueuePG_CompleteRelease_AtomicAcrossChunks is the pgx counterpart of
// TestQueue_CompleteRelease_AtomicAcrossChunks: it proves the chunked
// Complete/Release rolls back entirely when a chunk after the first fails,
// so the operation is all-or-nothing on PostgreSQL too.
func TestQueuePG_CompleteRelease_AtomicAcrossChunks(t *testing.T) {
	ctx := context.Background()

	origChunk := completeReleaseChunkRows
	completeReleaseChunkRows = 2
	t.Cleanup(func() { completeReleaseChunkRows = origChunk })

	injected := errors.New("injected mid-batch failure")
	t.Cleanup(func() { afterChunkHook = nil })

	// --- Complete (DELETE) atomicity ---
	const n = 5 // 5 ids over chunk size 2 → 3 chunks
	db := openPGQueueDB(t, n)
	q := NewQueue(db, pgRebind())

	ids, token, err := q.Claim(ctx, 1, n)
	require.NoError(t, err, "Claim")
	require.Len(t, ids, n)

	afterChunkHook = func(int) error { return injected }
	err = q.Complete(ctx, 1, token, ids)
	require.Error(t, err, "Complete must surface the injected failure")
	require.ErrorIs(t, err, injected, "error must wrap the injected cause")
	afterChunkHook = nil

	var total int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM pending_embeddings`).Scan(&total))
	assert.Equal(t, n, total, "failed chunked Complete must delete zero rows (all-or-nothing)")

	require.NoError(t, q.Complete(ctx, 1, token, ids), "retry Complete")
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM pending_embeddings`).Scan(&total))
	assert.Equal(t, 0, total, "clean retry deletes all rows")

	// --- Release (UPDATE) atomicity ---
	db2 := openPGQueueDB(t, n)
	q2 := NewQueue(db2, pgRebind())

	ids2, token2, err := q2.Claim(ctx, 1, n)
	require.NoError(t, err, "Claim (release case)")
	require.Len(t, ids2, n)
	require.Equal(t, 0, pgCountAvailable(t, db2), "all claimed before release")

	afterChunkHook = func(int) error { return injected }
	err = q2.Release(ctx, 1, token2, ids2)
	require.Error(t, err, "Release must surface the injected failure")
	require.ErrorIs(t, err, injected, "error must wrap the injected cause")
	afterChunkHook = nil

	assert.Equal(t, 0, pgCountAvailable(t, db2),
		"failed chunked Release must clear zero claims (all-or-nothing)")

	require.NoError(t, q2.Release(ctx, 1, token2, ids2), "retry Release")
	assert.Equal(t, n, pgCountAvailable(t, db2), "clean retry releases all rows")
}

func TestQueuePG_Complete_AfterReclaim_PreservesNewClaim(t *testing.T) {
	ctx := context.Background()
	db := openPGQueueDB(t, 2)
	q := NewQueue(db, pgRebind())

	idsA, tokenA, err := q.Claim(ctx, 1, 2)
	require.NoError(t, err, "Claim A")
	require.Len(t, idsA, 2)

	_, err = db.ExecContext(ctx,
		`UPDATE pending_embeddings SET claimed_at = $1 WHERE generation_id = 1`,
		time.Now().Add(-20*time.Minute).Unix())
	require.NoError(t, err)
	n, err := q.ReclaimStale(ctx, 10*time.Minute)
	require.NoError(t, err, "ReclaimStale")
	require.Equal(t, 2, n, "ReclaimStale count")

	idsB, tokenB, err := q.Claim(ctx, 1, 2)
	require.NoError(t, err, "Claim B")
	require.Len(t, idsB, 2)
	require.NotEqual(t, tokenA, tokenB)

	require.NoError(t, q.Complete(ctx, 1, tokenA, idsA), "Complete(stale tokenA)")
	var remaining int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM pending_embeddings`).Scan(&remaining))
	require.Equal(t, 2, remaining, "stale token must not delete")

	var claimed int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM pending_embeddings WHERE claim_token = $1`, tokenB).Scan(&claimed))
	assert.Equal(t, 2, claimed, "rows still holding B's token")

	require.NoError(t, q.Complete(ctx, 1, tokenB, idsB), "Complete(tokenB)")
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM pending_embeddings`).Scan(&remaining))
	assert.Equal(t, 0, remaining, "pending rows after B's Complete")
}

// TestQueuePG_ConcurrentClaim_SkipLocked verifies that FOR UPDATE SKIP LOCKED
// prevents two concurrent claimers from double-claiming the same rows. Each
// claimer runs on a separate *sql.DB (independent connection pool) so the
// claims are genuinely concurrent at the database level.
//
// The test inserts N pending rows, then fires two goroutines each calling
// Claim(N) concurrently. Because SKIP LOCKED makes the two transactions see
// disjoint available sets, the union of their claimed IDs must equal exactly
// the N inserted rows with no overlaps.
func TestQueuePG_ConcurrentClaim_SkipLocked(t *testing.T) {
	const n = 20
	ctx := context.Background()

	// One isolated schema seeded with n pending rows. newPGQueueSchema returns
	// the schema-scoped DSN so the second handle below targets the SAME schema
	// — essential for this test, since two different schemas would make the
	// SKIP-LOCKED assertion pass vacuously (two queues over disjoint tables).
	db1, dsn := newPGQueueSchema(t, n)

	// Open a second independent connection on the SAME schema so the two Queue
	// instances use separate connection pools and their transactions do not
	// share state.
	db2, err := sql.Open("pgx", dsn)
	require.NoError(t, err, "open db2")
	t.Cleanup(func() { _ = db2.Close() })

	// Guard against an accidental two-schema refactor: a row visible via db1
	// must also be visible via db2 (they resolve the same pending table).
	var visible int
	require.NoError(t,
		db2.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = 1`).Scan(&visible),
		"db2 must see db1's seeded rows (same schema)")
	require.Equal(t, n, visible, "db1 and db2 must target the same schema's pending table")

	q1 := NewQueue(db1, pgRebind())
	q2 := NewQueue(db2, pgRebind())

	type result struct {
		ids   []int64
		token string
		err   error
	}
	ch := make(chan result, 2)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		ids, tok, err := q1.Claim(ctx, 1, n)
		ch <- result{ids, tok, err}
	}()
	go func() {
		defer wg.Done()
		ids, tok, err := q2.Claim(ctx, 1, n)
		ch <- result{ids, tok, err}
	}()
	wg.Wait()
	close(ch)

	var allIDs []int64
	for res := range ch {
		require.NoError(t, res.err, "Claim must not error")
		allIDs = append(allIDs, res.ids...)
	}

	// The union of claimed IDs must equal exactly {1..n} with no duplicates.
	slices.Sort(allIDs)
	require.Len(t, allIDs, n, "total claimed rows must equal n (no rows unclaimed and no duplicates)")
	for i, id := range allIDs {
		assert.Equal(t, int64(i+1), id, "claimed ID at position %d", i)
	}
}
