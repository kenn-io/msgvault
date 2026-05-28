//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/vector"
)

func newBackendForTest(t *testing.T) (*Backend, context.Context) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vectors.db")
	main := openMainDBWithOneMessage(t)
	b, err := Open(ctx, Options{
		Path:      path,
		Dimension: 768,
		MainDB:    main,
	})
	requirepkg.NoError(t, err, "Open")
	t.Cleanup(func() { _ = b.Close() })
	return b, ctx
}

func TestBackend_CreateActivateRetire(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	b, ctx := newBackendForTest(t)

	gid, err := b.CreateGeneration(ctx, "nomic-embed-text-v1.5", 768, "")
	require.NoError(err, "CreateGeneration")

	bg, err := b.BuildingGeneration(ctx)
	require.NoError(err)
	require.NotNil(bg, "BuildingGeneration")
	require.Equal(gid, bg.ID)
	_, err = b.ActiveGeneration(ctx)
	require.Error(err, "ActiveGeneration should error before activation")

	require.NoError(b.ActivateGeneration(ctx, gid), "ActivateGeneration")
	g, err := b.ActiveGeneration(ctx)
	require.NoError(err, "ActiveGeneration after activate")
	assert.Equal(vector.GenerationActive, g.State)
	assert.Equal("nomic-embed-text-v1.5:768", g.Fingerprint)

	require.NoError(b.RetireGeneration(ctx, gid), "RetireGeneration")
	_, err = b.ActiveGeneration(ctx)
	require.Error(err, "ActiveGeneration should error after retire")
}

func TestBackend_CreateGeneration_SeedsPending(t *testing.T) {
	b, ctx := newBackendForTest(t)
	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	requirepkg.NoError(t, err, "Create")
	var n int
	err = b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ?`, gid,
	).Scan(&n)
	requirepkg.NoError(t, err, "count pending")
	assertpkg.Equal(t, 1, n, "pending count")
}

// TestBackend_CreateGeneration_ResumesBuilding confirms that calling
// CreateGeneration while a building row already exists with the same
// fingerprint returns the existing id instead of failing on the unique
// index. This makes retries after a crash idempotent.
func TestBackend_CreateGeneration_ResumesBuilding(t *testing.T) {
	b, ctx := newBackendForTest(t)

	first, err := b.CreateGeneration(ctx, "m", 768, "")
	requirepkg.NoError(t, err, "first Create")

	second, err := b.CreateGeneration(ctx, "m", 768, "")
	requirepkg.NoError(t, err, "second Create with matching fingerprint")
	assertpkg.Equal(t, first, second, "should reuse existing id")
}

// TestBackend_CreateGeneration_MismatchedFingerprint checks that a
// second CreateGeneration call with a different fingerprint while
// another build is in progress surfaces an actionable error wrapping
// vector.ErrBuildingInProgress, instead of a raw SQLite uniqueness
// error.
func TestBackend_CreateGeneration_MismatchedFingerprint(t *testing.T) {
	b, ctx := newBackendForTest(t)

	_, err := b.CreateGeneration(ctx, "model-a", 768, "")
	requirepkg.NoError(t, err, "first Create")

	_, err = b.CreateGeneration(ctx, "model-b", 768, "")
	requirepkg.Error(t, err, "second Create with different fingerprint")
	assertpkg.ErrorIs(t, err, vector.ErrBuildingInProgress)
}

// TestBackend_CreateGeneration_ResumeDoesNotReseedCompleted is the
// regression test for the "interrupted full rebuild re-embeds
// everything" bug: after the worker has already embedded some messages
// (Queue.Complete removed those rows from pending_embeddings), a
// retry'd CreateGeneration must NOT push them back onto the queue. We
// simulate this by manually removing a pending row, then calling
// CreateGeneration again with the same fingerprint and asserting the
// removed row is not re-enqueued.
func TestBackend_CreateGeneration_ResumeDoesNotReseedCompleted(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	b, ctx := newBackendForTest(t)

	gen, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(err, "first Create")

	// Simulate Queue.Complete: remove the pending row for the only
	// pre-seeded message (id=1) as if it were already embedded.
	_, err = b.db.ExecContext(ctx,
		`DELETE FROM pending_embeddings WHERE generation_id = ? AND message_id = ?`,
		int64(gen), int64(1))
	require.NoError(err, "delete pending")

	// Resume: CreateGeneration must reuse the existing building gen
	// and NOT re-enqueue the completed message.
	resumed, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(err, "resume Create")
	assert.Equal(gen, resumed, "resumed gen should reuse existing")
	var pending int
	err = b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ? AND message_id = 1`,
		int64(gen)).Scan(&pending)
	require.NoError(err, "count pending")
	assert.Equal(0, pending, "resume re-seeded a completed message")
}

// TestBackend_CreateGeneration_ResumeReseedsUnseededGeneration covers
// the "crash between row insert and seed commit" path: a building row
// exists but seeded_at is NULL because the previous attempt died
// before the seed transaction committed. A naive resume would skip
// seedPending, leave pending_embeddings empty, and let
// `msgvault embeddings build` activate the unseeded generation — silently
// replacing the prior active index with an empty one. The fix is to
// re-run seedPending whenever seeded_at IS NULL on resume.
func TestBackend_CreateGeneration_ResumeReseedsUnseededGeneration(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	b, ctx := newBackendForTest(t)

	gen, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(err, "first Create")
	// Simulate the crash window: clear seeded_at AND wipe the seeded
	// rows so the post-resume pending count is exactly what the resume
	// re-seed would produce. Without this we couldn't distinguish
	// "rows are present because resume re-seeded" from "rows are
	// present because the original seed left them there".
	_, err = b.db.ExecContext(ctx,
		`UPDATE index_generations SET seeded_at = NULL WHERE id = ?`, int64(gen))
	require.NoError(err, "clear seeded_at")
	_, err = b.db.ExecContext(ctx,
		`DELETE FROM pending_embeddings WHERE generation_id = ?`, int64(gen))
	require.NoError(err, "clear pending")

	resumed, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(err, "resume Create")
	assert.Equal(gen, resumed, "resumed gen should reuse existing")
	var pending int
	err = b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ?`,
		int64(gen)).Scan(&pending)
	require.NoError(err, "count pending")
	assert.Equal(1, pending, "resume must re-seed an unseeded build")
	// And seeded_at should now be populated so a second resume
	// would correctly skip re-seeding.
	var seededAt sql.NullInt64
	err = b.db.QueryRowContext(ctx,
		`SELECT seeded_at FROM index_generations WHERE id = ?`, int64(gen)).Scan(&seededAt)
	require.NoError(err, "read seeded_at")
	assert.True(seededAt.Valid, "seeded_at still NULL after resume re-seed; second resume would re-seed again")
}

// TestBackend_ClaimOrInsertBuilding_RaceRecoversFromUniqueConstraint
// exercises the post-INSERT unique-constraint recovery path: when a
// concurrent writer slips a building row in between our SELECT and
// INSERT, the partial unique index on (state) WHERE state='building'
// rejects the second writer. We must re-read the existing row and
// return its id (clean resume) rather than surfacing the raw SQLite
// error. We can't easily race two real callers in a single test, so
// we drive the helper directly: pre-insert a building row, then call
// claimOrInsertBuilding with the same fingerprint via a mocked
// "select returns no row" by using a fresh connection mid-flight.
//
// The simpler, deterministic guard: invoke claimOrInsertBuilding
// twice with matching fingerprints and confirm the second call
// returns isNew=false even after the first has committed. The
// dedicated race path is covered indirectly because both code paths
// converge on lookupBuilding.
func TestBackend_ClaimOrInsertBuilding_RecoversFromExistingRow(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	b, ctx := newBackendForTest(t)

	gen1, isNew1, err := b.claimOrInsertBuilding(ctx, "m", 768, "m:768", time.Now().Unix())
	require.NoError(err, "first claim")
	assert.True(isNew1, "first claim: isNew")

	// Second claim must reuse the row (isNew=false), and the path
	// would have hit the unique constraint had we tried INSERT first
	// without the SELECT. The recovery branch is what guarantees we
	// don't surface a raw SQLite error if some other writer wins.
	gen2, isNew2, err := b.claimOrInsertBuilding(ctx, "m", 768, "m:768", time.Now().Unix())
	require.NoError(err, "second claim")
	assert.False(isNew2, "second claim: existing row should be reused")
	assert.Equal(gen1, gen2, "should reuse gen id")
}

// TestBackend_CreateGeneration_SeedCommitsVisibleFirst confirms the
// new building row is committed *before* the seed pass runs, so a
// concurrent Enqueuer can see the generation and dual-enqueue
// newly-synced messages. Without this ordering there is a window
// during which sync-side enqueues would be scoped only to the active
// generation and the new build would be missing messages.
//
// The previous version of this test polled on a short loop and
// passed even if visibility happened only AFTER CreateGeneration
// returned, because <-done would block until the goroutine finished
// and the polling loop would then see the committed row regardless.
// We now seed many messages to make seedPending take measurable time
// and require visibility to be observed strictly while the goroutine
// is still in flight (done has not fired yet).
func TestBackend_CreateGeneration_SeedCommitsVisibleFirst(t *testing.T) {
	require := requirepkg.New(t)
	ctx := context.Background()

	// Build a backend whose main DB has many messages so seedPending
	// has enough work that we can race a visibility poll against it.
	// 5_000 rows is comfortably more than the one row in the standard
	// helper and drives seedPending into the millisecond range even on
	// a fast laptop — far longer than the polling interval below.
	main := openMainDBWithOneMessage(t)
	insert, err := main.PrepareContext(ctx, `INSERT INTO messages (id) VALUES (?)`)
	require.NoError(err, "prepare insert")
	defer func() { _ = insert.Close() }()
	for i := int64(2); i <= 5000; i++ {
		_, err := insert.ExecContext(ctx, i)
		require.NoErrorf(err, "insert msg %d", i)
	}

	b, err := Open(ctx, Options{
		Path:      filepath.Join(t.TempDir(), "vectors.db"),
		Dimension: 768,
		MainDB:    main,
	})
	require.NoError(err, "Open")
	t.Cleanup(func() { _ = b.Close() })

	done := make(chan error, 1)
	go func() {
		_, err := b.CreateGeneration(ctx, "m", 768, "")
		done <- err
	}()

	// Poll for visibility, but strictly while the goroutine is still
	// in flight: every iteration first checks `done` via select-default,
	// and a poll that fires after `done` is closed counts as a failure
	// because we'd then be observing a row that was committed at any
	// point — including after return. With 5000 messages to seed, we
	// have hundreds of polling windows before CreateGeneration returns.
	deadline := time.Now().Add(5 * time.Second)
	var (
		visibleInFlight bool
		doneFiredFirst  bool
	)
poll:
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			// Push the result back so the post-loop assertion can
			// also read it. If we got here without observing the row
			// yet, that is a failure.
			done <- err
			doneFiredFirst = true
			break poll
		default:
		}
		var id int64
		qErr := b.db.QueryRowContext(ctx,
			`SELECT id FROM index_generations WHERE state = 'building'`).Scan(&id)
		if qErr == nil && id > 0 {
			visibleInFlight = true
			break poll
		}
		time.Sleep(1 * time.Millisecond)
	}

	require.NoError(<-done, "CreateGeneration")
	require.False(doneFiredFirst, "CreateGeneration returned before the building row became visible — commit was deferred to after seed")
	require.True(visibleInFlight, "building generation was never visible while CreateGeneration was in flight")
}

func TestBackend_CreateGeneration_SkipsDeletedMessages(t *testing.T) {
	b := openBackendWithOneDeletedMessage(t)
	t.Cleanup(func() { _ = b.Close() })
	ctx := context.Background()
	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	requirepkg.NoError(t, err, "Create")
	var n int
	_ = b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ?`, gid).Scan(&n)
	assertpkg.Equal(t, 0, n, "pending count for deleted message")
}

// TestBackend_SeedPending_SkipsDedupHidden verifies that seedPending
// omits messages soft-deleted by dedup (deleted_at IS NOT NULL).
func TestBackend_SeedPending_SkipsDedupHidden(t *testing.T) {
	require := requirepkg.New(t)
	t.Helper()
	ctx := context.Background()

	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(err, "open main")
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`CREATE TABLE messages (
		id INTEGER PRIMARY KEY,
		deleted_at DATETIME,
		deleted_from_source_at DATETIME
	)`)
	require.NoError(err, "create messages")
	// Insert one live and one dedup-hidden message.
	_, err = db.Exec(`INSERT INTO messages (id) VALUES (1)`)
	require.NoError(err, "insert live")
	_, err = db.Exec(`INSERT INTO messages (id, deleted_at) VALUES (2, CURRENT_TIMESTAMP)`)
	require.NoError(err, "insert dedup-hidden")

	b, err := Open(ctx, Options{
		Path:      t.TempDir() + "/vectors.db",
		Dimension: 768,
		MainDB:    db,
	})
	require.NoError(err, "Open")
	t.Cleanup(func() { _ = b.Close() })

	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(err, "CreateGeneration")
	var n int
	err = b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ?`, gid).Scan(&n)
	require.NoError(err, "count pending")
	assertpkg.Equal(t, 1, n, "dedup-hidden message must be excluded")
}

// TestBackend_Upsert_WritesEmbeddingAndVector verifies Upsert's
// contract: it writes the embeddings row and the dimension-specific
// vec0 row, and explicitly does NOT touch pending_embeddings. The
// queue is the sole owner of that table so that Queue.Complete's
// token check can prevent a stale worker from wiping a newer worker's
// claim.
func TestBackend_Upsert_WritesEmbeddingAndVector(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	b, ctx := newBackendForTest(t)
	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(err, "CreateGeneration")

	vec := make([]float32, 768)
	for i := range vec {
		vec[i] = 0.1
	}
	chunks := []vector.Chunk{{MessageID: 1, Vector: vec, SourceCharLen: 42}}
	require.NoError(b.Upsert(ctx, gid, chunks), "Upsert")

	var n int
	err = b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = ? AND message_id = 1`, gid).Scan(&n)
	require.NoError(err, "count embeddings")
	assert.Equal(1, n, "embeddings count")

	err = b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM vectors_vec_d768 v
		   JOIN embeddings e ON e.embedding_id = v.embedding_id
		  WHERE v.generation_id = ? AND e.message_id = 1`, gid).Scan(&n)
	require.NoError(err, "count vectors_vec_d768")
	assert.Equal(1, n, "vectors_vec_d768 count")

	// Pending row is still present — the queue owns that table and
	// only Queue.Complete may remove it.
	err = b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ? AND message_id = 1`, gid).Scan(&n)
	require.NoError(err, "count pending")
	assert.Equal(1, n, "Upsert must not touch pending_embeddings")
}

func TestBackend_Upsert_DimensionMismatch(t *testing.T) {
	b, ctx := newBackendForTest(t)
	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	requirepkg.NoError(t, err, "CreateGeneration")

	short := make([]float32, 64) // wrong dim
	err = b.Upsert(ctx, gid, []vector.Chunk{{MessageID: 1, Vector: short}})
	assertpkg.ErrorIs(t, err, vector.ErrDimensionMismatch)
}

func TestBackend_Upsert_EmptyChunks(t *testing.T) {
	require := requirepkg.New(t)
	b, ctx := newBackendForTest(t)
	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(err, "CreateGeneration")

	require.NoError(b.Upsert(ctx, gid, nil), "Upsert(nil)")
	require.NoError(b.Upsert(ctx, gid, []vector.Chunk{}), "Upsert(empty)")

	var n int
	err = b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = ?`, gid).Scan(&n)
	require.NoError(err, "count embeddings")
	assertpkg.Equal(t, 0, n, "embeddings count")
}

func TestBackend_Upsert_UnknownGeneration(t *testing.T) {
	b, ctx := newBackendForTest(t)

	vec := make([]float32, 768)
	err := b.Upsert(ctx, vector.GenerationID(9999), []vector.Chunk{{MessageID: 1, Vector: vec}})
	assertpkg.ErrorIs(t, err, vector.ErrUnknownGeneration)
}

func TestBackend_Upsert_MultiChunkAndTruncated(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	b, ctx := newBackendForTest(t)
	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(err, "CreateGeneration")

	vec1 := make([]float32, 768)
	vec2 := make([]float32, 768)
	for i := range vec1 {
		vec1[i] = 0.1
		vec2[i] = 0.2
	}
	chunks := []vector.Chunk{
		{MessageID: 1, Vector: vec1, SourceCharLen: 10, Truncated: true},
		{MessageID: 2, Vector: vec2, SourceCharLen: 20, Truncated: false},
	}
	require.NoError(b.Upsert(ctx, gid, chunks), "Upsert")

	var n int
	err = b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = ?`, gid).Scan(&n)
	require.NoError(err, "count embeddings")
	assert.Equal(2, n, "embeddings count")

	var trunc int
	err = b.db.QueryRowContext(ctx,
		`SELECT truncated FROM embeddings WHERE generation_id = ? AND message_id = 1`, gid).Scan(&trunc)
	require.NoError(err, "scan truncated msg 1")
	assert.Equal(1, trunc, "truncated for msg 1")
	err = b.db.QueryRowContext(ctx,
		`SELECT truncated FROM embeddings WHERE generation_id = ? AND message_id = 2`, gid).Scan(&trunc)
	require.NoError(err, "scan truncated msg 2")
	assert.Equal(0, trunc, "truncated for msg 2")

	err = b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM vectors_vec_d768 WHERE generation_id = ?`, gid).Scan(&n)
	require.NoError(err, "count vectors_vec_d768")
	assert.Equal(2, n, "vectors_vec_d768 count")
}

// TestBackend_Upsert_MultiChunkMessage exercises the new
// per-chunk-row layout: one upsert with two chunks for the same
// message id must produce two embeddings rows (with chunk_index 0
// and 1) and two vec0 rows, joined back through embedding_id.
func TestBackend_Upsert_MultiChunkMessage(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	b, ctx := newBackendForTest(t)
	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(err, "CreateGeneration")
	v0 := make([]float32, 768)
	v1 := make([]float32, 768)
	for i := range v0 {
		v0[i] = 0.25
		v1[i] = 0.75
	}
	require.NoError(b.Upsert(ctx, gid, []vector.Chunk{
		{MessageID: 7, ChunkIndex: 0, Vector: v0, SourceCharLen: 100,
			ChunkCharStart: 0, ChunkCharEnd: 100},
		{MessageID: 7, ChunkIndex: 1, Vector: v1, SourceCharLen: 90,
			ChunkCharStart: 80, ChunkCharEnd: 170},
	}), "Upsert")
	var n int
	err = b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = ? AND message_id = 7`, gid).Scan(&n)
	require.NoError(err, "count embeddings")
	assert.Equal(2, n, "embeddings rows")
	// Each chunk_index appears exactly once.
	err = b.db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT chunk_index) FROM embeddings WHERE generation_id = ? AND message_id = 7`, gid).Scan(&n)
	require.NoError(err, "count distinct chunk_index")
	assert.Equal(2, n, "distinct chunk_index")
	// vec0 has two rows, joined back through embedding_id.
	err = b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM vectors_vec_d768 v
		   JOIN embeddings e ON e.embedding_id = v.embedding_id
		  WHERE v.generation_id = ? AND e.message_id = 7`, gid).Scan(&n)
	require.NoError(err, "count vectors")
	assert.Equal(2, n, "vec rows")
	// message_count counts distinct messages, not chunks: a two-chunk
	// message contributes exactly one.
	err = b.db.QueryRowContext(ctx,
		`SELECT message_count FROM index_generations WHERE id = ?`, gid).Scan(&n)
	require.NoError(err, "read message_count")
	assert.Equal(1, n, "one distinct message")
}

// TestBackend_Upsert_ReplaceFewerChunks confirms idempotency when the
// chunk fan-out shrinks across upserts: re-upserting a message with
// only chunk 0 must remove the chunk 1 left from a previous call.
// Half-replace would leave an orphan row pointing at stale text.
func TestBackend_Upsert_ReplaceFewerChunks(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	b, ctx := newBackendForTest(t)
	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(err, "CreateGeneration")
	v0 := make([]float32, 768)
	v1 := make([]float32, 768)
	for i := range v0 {
		v0[i] = 0.1
		v1[i] = 0.9
	}
	// First upsert: two chunks.
	require.NoError(b.Upsert(ctx, gid, []vector.Chunk{
		{MessageID: 5, ChunkIndex: 0, Vector: v0, SourceCharLen: 100},
		{MessageID: 5, ChunkIndex: 1, Vector: v1, SourceCharLen: 90},
	}), "first Upsert")
	// Second upsert: only chunk 0. Idempotent replace should also
	// vacate the stale chunk 1 row.
	require.NoError(b.Upsert(ctx, gid, []vector.Chunk{
		{MessageID: 5, ChunkIndex: 0, Vector: v0, SourceCharLen: 999},
	}), "second Upsert")
	var n int
	err = b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = ? AND message_id = 5`, gid).Scan(&n)
	require.NoError(err, "count embeddings")
	assert.Equal(1, n, "chunk 1 should be vacated")
	err = b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM vectors_vec_d768 v
		   JOIN embeddings e ON e.embedding_id = v.embedding_id
		  WHERE v.generation_id = ? AND e.message_id = 5`, gid).Scan(&n)
	require.NoError(err, "count vectors")
	assert.Equal(1, n, "chunk 1 vec should be vacated")
}

func TestBackend_Upsert_ReplacesExisting(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	b, ctx := newBackendForTest(t)
	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(err, "CreateGeneration")

	vec1 := make([]float32, 768)
	for i := range vec1 {
		vec1[i] = 0.1
	}
	require.NoError(b.Upsert(ctx, gid, []vector.Chunk{{MessageID: 1, Vector: vec1, SourceCharLen: 10}}), "first Upsert")

	vec2 := make([]float32, 768)
	for i := range vec2 {
		vec2[i] = 0.9
	}
	require.NoError(b.Upsert(ctx, gid, []vector.Chunk{{MessageID: 1, Vector: vec2, SourceCharLen: 999}}), "second Upsert")

	var n int
	err = b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = ? AND message_id = 1`, gid).Scan(&n)
	require.NoError(err, "count embeddings")
	assert.Equal(1, n, "embeddings count")

	var charLen int
	err = b.db.QueryRowContext(ctx,
		`SELECT source_char_len FROM embeddings WHERE generation_id = ? AND message_id = 1`, gid).Scan(&charLen)
	require.NoError(err, "scan source_char_len")
	assert.Equal(999, charLen)

	err = b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM vectors_vec_d768 v
		   JOIN embeddings e ON e.embedding_id = v.embedding_id
		  WHERE v.generation_id = ? AND e.message_id = 1`, gid).Scan(&n)
	require.NoError(err, "count vectors_vec_d768")
	assert.Equal(1, n, "vectors_vec_d768 count")
}

func TestBackend_Search_ReturnsRankedHits(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	b, ctx := newBackendForTest(t)
	gid := seedAndEmbed(t, b, map[int64][]float32{
		10: unitVec(768, 0),
		11: unitVec(768, 1),
		12: unitVec(768, 2),
	})

	hits, err := b.Search(ctx, gid, unitVec(768, 1), 2, vector.Filter{})
	require.NoError(err, "Search")
	require.Len(hits, 2)
	assert.Equal(int64(11), hits[0].MessageID, "top hit")
	assert.Equal(1, hits[0].Rank, "top rank")
}

func TestBackend_Search_EmptyQueryVector(t *testing.T) {
	b, ctx := newBackendForTest(t)
	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	requirepkg.NoError(t, err, "CreateGeneration")
	_, err = b.Search(ctx, gid, nil, 5, vector.Filter{})
	requirepkg.Error(t, err, "Search with nil queryVec should error")
	_, err = b.Search(ctx, gid, []float32{}, 5, vector.Filter{})
	requirepkg.Error(t, err, "Search with empty queryVec should error")
}

func TestBackend_Search_UnknownGeneration(t *testing.T) {
	b, ctx := newBackendForTest(t)
	vec := unitVec(768, 0)
	_, err := b.Search(ctx, vector.GenerationID(9999), vec, 5, vector.Filter{})
	assertpkg.ErrorIs(t, err, vector.ErrUnknownGeneration)
}

func TestBackend_Search_DimensionMismatch(t *testing.T) {
	b, ctx := newBackendForTest(t)
	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	requirepkg.NoError(t, err, "CreateGeneration")
	_, err = b.Search(ctx, gid, unitVec(64, 0), 5, vector.Filter{})
	assertpkg.ErrorIs(t, err, vector.ErrDimensionMismatch)
}

// TestBackend_Search_FilterIDsExceedSQLiteParamCap exercises the
// json_each path in resolveFilter with a filter that resolves to more
// messages than SQLite's ~999 practical bound-parameter cap. The old
// implementation expanded the id set into one `IN (?,?,...)` list per
// id and failed with `too many SQL variables` once it crossed the cap.
func TestBackend_Search_FilterIDsExceedSQLiteParamCap(t *testing.T) {
	require := requirepkg.New(t)
	b, ctx, _ := newFusedBackendForTest(t)

	const total = 1200 // well past SQLite's 999-variable ceiling
	// The helper seeds 3 FTS rows; insert `total` more messages each
	// with a `from` recipient row pointing at the same participant so
	// a single sender filter matches all of them.
	_, err := b.mainDB.ExecContext(ctx,
		`DELETE FROM messages; DELETE FROM messages_fts; DELETE FROM message_recipients`)
	require.NoError(err, "reset main")
	insertMsg, err := b.mainDB.PrepareContext(ctx,
		`INSERT INTO messages (id) VALUES (?)`)
	require.NoError(err, "prepare msg")
	defer func() { _ = insertMsg.Close() }()
	insertMR, err := b.mainDB.PrepareContext(ctx,
		`INSERT INTO message_recipients (message_id, recipient_type, participant_id) VALUES (?, 'from', 42)`)
	require.NoError(err, "prepare mr")
	defer func() { _ = insertMR.Close() }()
	vecs := make(map[int64][]float32, total)
	for i := int64(1); i <= total; i++ {
		_, err := insertMsg.ExecContext(ctx, i)
		require.NoErrorf(err, "insert %d", i)
		_, err = insertMR.ExecContext(ctx, i)
		require.NoErrorf(err, "insert mr %d", i)
		vecs[i] = unitVec(768, 0)
	}

	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(err, "CreateGeneration")
	// Upsert a few chunks so Search has something to rank. We don't
	// need all `total` embedded — the filter is what we're stressing.
	chunks := make([]vector.Chunk, 0, 5)
	for i := int64(1); i <= 5; i++ {
		chunks = append(chunks, vector.Chunk{MessageID: i, Vector: vecs[i]})
	}
	require.NoError(b.Upsert(ctx, gid, chunks), "Upsert")

	hits, err := b.Search(ctx, gid, unitVec(768, 0), 3, vector.Filter{SenderGroups: [][]int64{{42}}})
	require.NoErrorf(err, "Search with broad filter (%d ids)", total)
	assertpkg.NotEmpty(t, hits, "expected at least one hit after filter")
}

// TestBackend_Search_NewFilterFields exercises the filter fields added
// to match the existing SQLite search surface: to/cc/bcc recipients,
// larger/smaller size bounds, and subject substring match.
func TestBackend_Search_NewFilterFields(t *testing.T) {
	require := requirepkg.New(t)
	b, ctx, _ := newFusedBackendForTest(t)

	// Reset and seed 4 messages with distinct recipient / size / subject
	// profiles so each assertion is unambiguous.
	_, err := b.mainDB.ExecContext(ctx,
		`DELETE FROM messages; DELETE FROM messages_fts; DELETE FROM message_recipients`)
	require.NoError(err, "reset")

	rows := []struct {
		id      int64
		size    int64
		subject string
		to, cc  int64
	}{
		{1, 100_000, "quarterly planning", 10, 0},
		{2, 5_000_000, "quarterly review", 20, 10},
		{3, 100_000, "lunch", 20, 0},
		{4, 20_000_000, "quarterly deep dive", 30, 0},
	}
	for _, r := range rows {
		_, err := b.mainDB.ExecContext(ctx,
			`INSERT INTO messages (id, subject, size_estimate) VALUES (?, ?, ?)`,
			r.id, r.subject, r.size)
		require.NoErrorf(err, "insert msg %d", r.id)
		if r.to != 0 {
			_, err := b.mainDB.ExecContext(ctx,
				`INSERT INTO message_recipients (message_id, recipient_type, participant_id)
				 VALUES (?, 'to', ?)`, r.id, r.to)
			require.NoError(err, "insert to")
		}
		if r.cc != 0 {
			_, err := b.mainDB.ExecContext(ctx,
				`INSERT INTO message_recipients (message_id, recipient_type, participant_id)
				 VALUES (?, 'cc', ?)`, r.id, r.cc)
			require.NoError(err, "insert cc")
		}
	}

	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(err, "CreateGeneration")
	chunks := make([]vector.Chunk, 0, len(rows))
	for _, r := range rows {
		chunks = append(chunks, vector.Chunk{MessageID: r.id, Vector: unitVec(768, 0)})
	}
	require.NoError(b.Upsert(ctx, gid, chunks), "Upsert")

	matched := func(t *testing.T, f vector.Filter) map[int64]bool {
		t.Helper()
		hits, err := b.Search(ctx, gid, unitVec(768, 0), 10, f)
		require.NoError(err, "Search")
		got := make(map[int64]bool, len(hits))
		for _, h := range hits {
			got[h.MessageID] = true
		}
		return got
	}

	t.Run("ToGroups_singleGroup", func(t *testing.T) {
		got := matched(t, vector.Filter{ToGroups: [][]int64{{20}}})
		assertpkg.Truef(t, got[2] && got[3] && !got[1] && !got[4], "ToGroups=[[20]]: got %v, want {2,3}", got)
	})
	t.Run("CcGroups_singleGroup", func(t *testing.T) {
		got := matched(t, vector.Filter{CcGroups: [][]int64{{10}}})
		assertpkg.Truef(t, got[2] && !got[1] && !got[3] && !got[4], "CcGroups=[[10]]: got %v, want {2}", got)
	})
	t.Run("LargerThan", func(t *testing.T) {
		size := int64(1_000_000)
		got := matched(t, vector.Filter{LargerThan: &size})
		assertpkg.Truef(t, got[2] && got[4] && !got[1] && !got[3], "LargerThan=1MB: got %v, want {2,4}", got)
	})
	t.Run("SmallerThan", func(t *testing.T) {
		size := int64(1_000_000)
		got := matched(t, vector.Filter{SmallerThan: &size})
		assertpkg.Truef(t, got[1] && got[3] && !got[2] && !got[4], "SmallerThan=1MB: got %v, want {1,3}", got)
	})
	t.Run("SubjectSubstring", func(t *testing.T) {
		got := matched(t, vector.Filter{SubjectSubstrings: []string{"quarterly"}})
		assertpkg.Truef(t, got[1] && got[2] && got[4] && !got[3], "subject=quarterly: got %v, want {1,2,4}", got)
	})
	t.Run("MultipleSubjectsANDed", func(t *testing.T) {
		got := matched(t, vector.Filter{SubjectSubstrings: []string{"quarterly", "deep"}})
		assertpkg.Truef(t, got[4] && !got[1] && !got[2] && !got[3], "subject=[quarterly, deep]: got %v, want {4}", got)
	})
	t.Run("CombinedFilter", func(t *testing.T) {
		size := int64(1_000_000)
		got := matched(t, vector.Filter{
			ToGroups:          [][]int64{{20}},
			LargerThan:        &size,
			SubjectSubstrings: []string{"quarterly"},
		})
		assertpkg.Truef(t, got[2] && !got[1] && !got[3] && !got[4], "combined to=20 + >1MB + quarterly: got %v, want {2}", got)
	})
}

// TestBackend_Search_RecipientGroupsAreANDed asserts that multiple
// groups for the same recipient field require the message to match
// EVERY group — i.e. `to:alice to:bob` is NOT the same as
// `to:(alice OR bob)`. Each group becomes its own EXISTS clause and
// they are AND'd together. Same shape as label group AND'ing.
func TestBackend_Search_RecipientGroupsAreANDed(t *testing.T) {
	require := requirepkg.New(t)
	b, ctx, _ := newFusedBackendForTest(t)

	_, err := b.mainDB.ExecContext(ctx,
		`DELETE FROM messages; DELETE FROM messages_fts; DELETE FROM message_recipients; DELETE FROM message_labels`)
	require.NoError(err, "reset")

	// Three messages, distinguishable by recipient set:
	//   1: to=100 only
	//   2: to=100, to=200       <- matches both groups
	//   3: to=200 only
	rows := []struct {
		id  int64
		tos []int64
	}{
		{1, []int64{100}},
		{2, []int64{100, 200}},
		{3, []int64{200}},
	}
	for _, r := range rows {
		_, err := b.mainDB.ExecContext(ctx,
			`INSERT INTO messages (id) VALUES (?)`, r.id)
		require.NoErrorf(err, "insert msg %d", r.id)
		for _, p := range r.tos {
			_, err := b.mainDB.ExecContext(ctx,
				`INSERT INTO message_recipients (message_id, recipient_type, participant_id)
				 VALUES (?, 'to', ?)`, r.id, p)
			require.NoError(err, "insert to")
		}
	}
	// Seed message_labels with the same shape: msg 2 has both labels,
	// msg 1 only label_id=1, msg 3 only label_id=2. The backend's filter
	// goes straight to message_labels (no labels-table join), so raw
	// label_ids are sufficient.
	for _, ml := range []struct {
		mid int64
		lid int64
	}{
		{1, 1},
		{2, 1}, {2, 2},
		{3, 2},
	} {
		_, err := b.mainDB.ExecContext(ctx,
			`INSERT INTO message_labels (message_id, label_id) VALUES (?, ?)`,
			ml.mid, ml.lid)
		require.NoError(err, "insert message_label")
	}

	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(err, "CreateGeneration")
	chunks := make([]vector.Chunk, 0, len(rows))
	for _, r := range rows {
		chunks = append(chunks, vector.Chunk{MessageID: r.id, Vector: unitVec(768, 0)})
	}
	require.NoError(b.Upsert(ctx, gid, chunks), "Upsert")

	matched := func(t *testing.T, f vector.Filter) map[int64]bool {
		t.Helper()
		hits, err := b.Search(ctx, gid, unitVec(768, 0), 10, f)
		require.NoError(err, "Search")
		got := make(map[int64]bool, len(hits))
		for _, h := range hits {
			got[h.MessageID] = true
		}
		return got
	}

	t.Run("two_to_groups_require_both", func(t *testing.T) {
		// `to:100 to:200` ⇒ ToGroups=[[100],[200]]; only msg 2 has both.
		got := matched(t, vector.Filter{ToGroups: [][]int64{{100}, {200}}})
		assertpkg.Truef(t, got[2] && !got[1] && !got[3], "ToGroups=[[100],[200]]: got %v, want only {2}", got)
	})

	t.Run("two_label_groups_require_both", func(t *testing.T) {
		// `label:1 label:2` ⇒ LabelGroups=[[1],[2]]; only msg 2 has both.
		got := matched(t, vector.Filter{LabelGroups: [][]int64{{1}, {2}}})
		assertpkg.Truef(t, got[2] && !got[1] && !got[3], "LabelGroups=[[1],[2]]: got %v, want only {2}", got)
	})

	t.Run("OR_within_a_group_still_works", func(t *testing.T) {
		// One group containing both ids ⇒ matches messages with either.
		got := matched(t, vector.Filter{ToGroups: [][]int64{{100, 200}}})
		assertpkg.Truef(t, got[1] && got[2] && got[3], "ToGroups=[[100,200]]: got %v, want {1,2,3}", got)
	})
}

// TestBackend_Search_SenderMatchesFromRecipientOnly confirms that
// SenderGroups filters match strictly against `from` recipient rows
// (matching internal/store/api.go:327-336). Messages whose only sender
// record is `messages.sender_id` do NOT match, because letting
// sender_id also satisfy sender filters would diverge from the SQLite
// path and allow repeated `from:` tokens to be satisfied by a mix of
// sender_id and recipient rows.
func TestBackend_Search_SenderMatchesFromRecipientOnly(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	b, ctx, _ := newFusedBackendForTest(t)

	// Reset the fused helper's seed data so we control the rows.
	_, err := b.mainDB.ExecContext(ctx,
		`DELETE FROM messages; DELETE FROM messages_fts; DELETE FROM message_recipients`)
	require.NoError(err, "reset main")

	// msg 1: sender_id=100, NO `from` recipient row → must NOT match.
	_, err = b.mainDB.ExecContext(ctx,
		`INSERT INTO messages (id, sender_id) VALUES (1, 100)`)
	require.NoError(err, "insert msg 1")
	// msg 2: no sender_id, `from` recipient row with pid=100 → matches.
	_, err = b.mainDB.ExecContext(ctx,
		`INSERT INTO messages (id) VALUES (2)`)
	require.NoError(err, "insert msg 2")
	_, err = b.mainDB.ExecContext(ctx,
		`INSERT INTO message_recipients (message_id, recipient_type, participant_id)
		 VALUES (2, 'from', 100)`)
	require.NoError(err, "insert mr")
	// msg 3: different sender (`from` row for pid=999) → must NOT match.
	_, err = b.mainDB.ExecContext(ctx,
		`INSERT INTO messages (id) VALUES (3)`)
	require.NoError(err, "insert msg 3")
	_, err = b.mainDB.ExecContext(ctx,
		`INSERT INTO message_recipients (message_id, recipient_type, participant_id)
		 VALUES (3, 'from', 999)`)
	require.NoError(err, "insert mr 3")

	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(err, "CreateGeneration")
	chunks := []vector.Chunk{
		{MessageID: 1, Vector: unitVec(768, 0)},
		{MessageID: 2, Vector: unitVec(768, 0)},
		{MessageID: 3, Vector: unitVec(768, 0)},
	}
	require.NoError(b.Upsert(ctx, gid, chunks), "Upsert")

	hits, err := b.Search(ctx, gid, unitVec(768, 0), 10, vector.Filter{SenderGroups: [][]int64{{100}}})
	require.NoError(err, "Search")
	got := make(map[int64]bool)
	for _, h := range hits {
		got[h.MessageID] = true
	}
	assert.False(got[1], "msg 1 (sender_id=100 without `from` recipient row must not match)")
	assert.True(got[2], "msg 2 (`from` recipient row pid=100)")
	assert.False(got[3], "msg 3 (different `from` recipient)")
}

// TestBackend_Search_SenderGroupsAreANDed_AtMessageLevel asserts that
// repeated `from:` operators are AND'd at the message level — a
// message with two `from` recipient rows can satisfy two `from:`
// tokens even though messages.sender_id is single-valued. This
// matches internal/store/api.go's behavior for repeated `from:` and
// regression-guards the bug where SenderGroups were collapsed to a
// participant-level intersection (which would drop such messages).
func TestBackend_Search_SenderGroupsAreANDed_AtMessageLevel(t *testing.T) {
	require := requirepkg.New(t)
	b, ctx, _ := newFusedBackendForTest(t)

	_, err := b.mainDB.ExecContext(ctx,
		`DELETE FROM messages; DELETE FROM messages_fts; DELETE FROM message_recipients`)
	require.NoError(err, "reset")

	// Three messages, each seeded with explicit `from` recipient rows.
	// Sender-group filtering resolves against those rows only (matching
	// the SQLite FTS path), so `from:100 from:200` requires two
	// distinct `from` rows on the same message.
	//   1: `from` rows {100}           — matches group [100] only
	//   2: `from` rows {100, 200}      — matches both groups
	//   3: `from` rows {100, 200}      — matches both groups
	_, err = b.mainDB.ExecContext(ctx,
		`INSERT INTO messages (id) VALUES (1), (2), (3)`)
	require.NoError(err, "insert messages")
	for _, mr := range []struct {
		mid int64
		pid int64
	}{
		{1, 100},
		{2, 100}, {2, 200},
		{3, 100}, {3, 200},
	} {
		_, err := b.mainDB.ExecContext(ctx,
			`INSERT INTO message_recipients (message_id, recipient_type, participant_id)
			 VALUES (?, 'from', ?)`, mr.mid, mr.pid)
		require.NoError(err, "insert mr")
	}

	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(err, "CreateGeneration")
	chunks := []vector.Chunk{
		{MessageID: 1, Vector: unitVec(768, 0)},
		{MessageID: 2, Vector: unitVec(768, 0)},
		{MessageID: 3, Vector: unitVec(768, 0)},
	}
	require.NoError(b.Upsert(ctx, gid, chunks), "Upsert")

	matched := func(t *testing.T, f vector.Filter) map[int64]bool {
		t.Helper()
		hits, err := b.Search(ctx, gid, unitVec(768, 0), 10, f)
		require.NoError(err, "Search")
		got := make(map[int64]bool, len(hits))
		for _, h := range hits {
			got[h.MessageID] = true
		}
		return got
	}

	t.Run("two_groups_AND_at_message_level", func(t *testing.T) {
		got := matched(t, vector.Filter{SenderGroups: [][]int64{{100}, {200}}})
		assertpkg.Truef(t, !got[1] && got[2] && got[3], "SenderGroups=[[100],[200]]: got %v, want {2,3}", got)
	})

	t.Run("single_group_OR_within", func(t *testing.T) {
		got := matched(t, vector.Filter{SenderGroups: [][]int64{{100, 200}}})
		assertpkg.Truef(t, got[1] && got[2] && got[3], "SenderGroups=[[100,200]]: got %v, want {1,2,3}", got)
	})
}

// TestBackend_Search_ExcludesDeletedFromSource regresses the bug
// where Backend.Search with an empty filter bypassed the deletion
// check and returned hits for messages whose deleted_from_source_at
// is set. This affected mode=vector and find_similar_messages, both
// of which call Backend.Search without a structured filter. The
// hybrid path (FusedSearch) was unaffected because its CTE
// hardcodes the same check, but the parity gap meant pure-vector
// answers could include archive-deleted messages.
func TestBackend_Search_ExcludesDeletedFromSource(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	b, ctx, _ := newFusedBackendForTest(t)

	_, err := b.mainDB.ExecContext(ctx,
		`DELETE FROM messages; DELETE FROM messages_fts; DELETE FROM message_recipients`)
	require.NoError(err, "reset")

	// Two messages: 1 live, 2 soft-deleted.
	_, err = b.mainDB.ExecContext(ctx,
		`INSERT INTO messages (id, deleted_from_source_at) VALUES (1, NULL), (2, '2026-01-01 00:00:00')`)
	require.NoError(err, "insert messages")

	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(err, "CreateGeneration")
	chunks := []vector.Chunk{
		{MessageID: 1, Vector: unitVec(768, 0)},
		{MessageID: 2, Vector: unitVec(768, 0)},
	}
	require.NoError(b.Upsert(ctx, gid, chunks), "Upsert")

	// Empty filter: must still exclude the soft-deleted message.
	hits, err := b.Search(ctx, gid, unitVec(768, 0), 10, vector.Filter{})
	require.NoError(err, "Search (empty filter)")
	got := make(map[int64]bool, len(hits))
	for _, h := range hits {
		got[h.MessageID] = true
	}
	assert.True(got[1], "msg 1 (not deleted, must appear)")
	assert.False(got[2], "msg 2 (deleted_from_source_at IS NOT NULL, must be excluded)")
}

// TestBackend_Search_OverFetchesToHonorKWhenTopHitsDeleted regresses
// the case where soft-deleted messages occupy slots in the top-k of
// the raw ANN result. Post-filtering deletions after fetching exactly
// k hits shrank the returned slice below k even when plenty more live
// neighbors existed just below the cutoff. The fast path must
// over-fetch enough to still return k live hits in this situation.
func TestBackend_Search_OverFetchesToHonorKWhenTopHitsDeleted(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	b, ctx, _ := newFusedBackendForTest(t)

	_, err := b.mainDB.ExecContext(ctx,
		`DELETE FROM messages; DELETE FROM messages_fts; DELETE FROM message_recipients`)
	require.NoError(err, "reset")

	// Seed 8 messages: 1–3 are soft-deleted and embedded at the exact
	// query vector (distance 0), 4–8 are live and embedded at
	// successively more distant perturbations. With k=5 and the old
	// "fetch k, post-filter" strategy, sqlite-vec's top-5 would be
	// {1,2,3,4,5}; dropping the deleted rows left only {4,5}. The
	// over-fetch fix should now return 5 live hits.
	_, err = b.mainDB.ExecContext(ctx, `
		INSERT INTO messages (id, deleted_from_source_at) VALUES
		    (1, '2026-01-01'), (2, '2026-01-01'), (3, '2026-01-01'),
		    (4, NULL), (5, NULL), (6, NULL), (7, NULL), (8, NULL)`)
	require.NoError(err, "insert messages")

	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(err, "CreateGeneration")

	// Distance grows with the live-message id so ANN order is
	// 1,2,3 (deleted, distance 0), then 4,5,6,7,8.
	gradedVec := func(offset float32) []float32 {
		v := unitVec(768, 0)
		v[1] = offset
		return v
	}
	chunks := []vector.Chunk{
		{MessageID: 1, Vector: unitVec(768, 0)},
		{MessageID: 2, Vector: unitVec(768, 0)},
		{MessageID: 3, Vector: unitVec(768, 0)},
		{MessageID: 4, Vector: gradedVec(0.01)},
		{MessageID: 5, Vector: gradedVec(0.02)},
		{MessageID: 6, Vector: gradedVec(0.03)},
		{MessageID: 7, Vector: gradedVec(0.04)},
		{MessageID: 8, Vector: gradedVec(0.05)},
	}
	require.NoError(b.Upsert(ctx, gid, chunks), "Upsert")

	hits, err := b.Search(ctx, gid, unitVec(768, 0), 5, vector.Filter{})
	require.NoError(err, "Search")
	require.Len(hits, 5, "over-fetch must absorb deletions")
	got := make(map[int64]bool, len(hits))
	for _, h := range hits {
		got[h.MessageID] = true
	}
	for _, deleted := range []int64{1, 2, 3} {
		assert.Falsef(got[deleted], "hits contain deleted msg %d", deleted)
	}
	for _, live := range []int64{4, 5, 6, 7, 8} {
		assert.Truef(got[live], "hits missing live msg %d (want top-5 live set {4,5,6,7,8}, got %v)", live, got)
	}
	// Ranks must be 1..5 in hit order (not the sparse ranks the
	// raw ANN query assigned).
	for i, h := range hits {
		assert.Equalf(i+1, h.Rank, "hit[%d].Rank (post-filter must re-number)", i)
	}
}

// TestBackend_Search_IterativelyExpandsWhenDeletionsExceedOverfetch
// locks in the fallback path: when soft-deleted messages occupy more
// than deletedOverfetchFactor * k of the top ANN hits, a single 2×
// over-fetch isn't enough. Search must keep doubling fetch until it
// collects k live hits or exhausts the generation.
func TestBackend_Search_IterativelyExpandsWhenDeletionsExceedOverfetch(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	b, ctx, _ := newFusedBackendForTest(t)

	_, err := b.mainDB.ExecContext(ctx,
		`DELETE FROM messages; DELETE FROM messages_fts; DELETE FROM message_recipients`)
	require.NoError(err, "reset")

	// Seed 6 deleted messages at distance 0 plus 5 live messages at
	// graded distances. With k=3, the opening 2× over-fetch of 6
	// returns only deleted rows (0 live). The iterative path must
	// double fetch to 12 and surface live hits {7,8,9}.
	_, err = b.mainDB.ExecContext(ctx, `
		INSERT INTO messages (id, deleted_from_source_at) VALUES
		    (1, '2026-01-01'), (2, '2026-01-01'), (3, '2026-01-01'),
		    (4, '2026-01-01'), (5, '2026-01-01'), (6, '2026-01-01'),
		    (7, NULL), (8, NULL), (9, NULL), (10, NULL), (11, NULL)`)
	require.NoError(err, "insert messages")

	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(err, "CreateGeneration")

	gradedVec := func(offset float32) []float32 {
		v := unitVec(768, 0)
		v[1] = offset
		return v
	}
	chunks := []vector.Chunk{
		{MessageID: 1, Vector: unitVec(768, 0)},
		{MessageID: 2, Vector: unitVec(768, 0)},
		{MessageID: 3, Vector: unitVec(768, 0)},
		{MessageID: 4, Vector: unitVec(768, 0)},
		{MessageID: 5, Vector: unitVec(768, 0)},
		{MessageID: 6, Vector: unitVec(768, 0)},
		{MessageID: 7, Vector: gradedVec(0.01)},
		{MessageID: 8, Vector: gradedVec(0.02)},
		{MessageID: 9, Vector: gradedVec(0.03)},
		{MessageID: 10, Vector: gradedVec(0.04)},
		{MessageID: 11, Vector: gradedVec(0.05)},
	}
	require.NoError(b.Upsert(ctx, gid, chunks), "Upsert")

	hits, err := b.Search(ctx, gid, unitVec(768, 0), 3, vector.Filter{})
	require.NoError(err, "Search")
	require.Len(hits, 3, "iterative expansion must cover >k deletions")
	wantIDs := map[int64]bool{7: true, 8: true, 9: true}
	for _, h := range hits {
		assert.Truef(wantIDs[h.MessageID], "unexpected hit id=%d (want any of {7,8,9})", h.MessageID)
	}
	for i, h := range hits {
		assert.Equalf(i+1, h.Rank, "hit[%d].Rank", i)
	}
}

// TestBackend_Search_ExhaustedCorpusReturnsWhatsAvailable guards the
// termination case: if k exceeds the number of live vectors even
// after expanding to the whole generation, Search returns the
// remainder without looping forever.
func TestBackend_Search_ExhaustedCorpusReturnsWhatsAvailable(t *testing.T) {
	require := requirepkg.New(t)
	b, ctx, _ := newFusedBackendForTest(t)

	_, err := b.mainDB.ExecContext(ctx,
		`DELETE FROM messages; DELETE FROM messages_fts; DELETE FROM message_recipients`)
	require.NoError(err, "reset")

	// Seed 3 deleted and 2 live messages. Request k=4: even the full
	// corpus sweep only produces 2 live hits, so Search must return 2
	// rather than loop.
	_, err = b.mainDB.ExecContext(ctx, `
		INSERT INTO messages (id, deleted_from_source_at) VALUES
		    (1, '2026-01-01'), (2, '2026-01-01'), (3, '2026-01-01'),
		    (4, NULL), (5, NULL)`)
	require.NoError(err, "insert messages")

	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(err, "CreateGeneration")
	chunks := []vector.Chunk{
		{MessageID: 1, Vector: unitVec(768, 0)},
		{MessageID: 2, Vector: unitVec(768, 0)},
		{MessageID: 3, Vector: unitVec(768, 0)},
		{MessageID: 4, Vector: unitVec(768, 1)},
		{MessageID: 5, Vector: unitVec(768, 2)},
	}
	require.NoError(b.Upsert(ctx, gid, chunks), "Upsert")

	hits, err := b.Search(ctx, gid, unitVec(768, 0), 4, vector.Filter{})
	require.NoError(err, "Search")
	require.Len(hits, 2, "only 2 live messages exist")
}

func TestBackend_Delete_RemovesFromAllTables(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	b, ctx := newBackendForTest(t)
	gid := seedAndEmbed(t, b, map[int64][]float32{1: unitVec(768, 0)})

	require.NoError(b.Delete(ctx, gid, []int64{1}), "Delete")
	var n int
	err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE message_id = 1`).Scan(&n)
	require.NoError(err, "count embeddings")
	assert.Equal(0, n, "embeddings remaining")
	err = b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM vectors_vec_d768`).Scan(&n)
	require.NoError(err, "count vectors")
	assert.Equal(0, n, "vectors remaining")
}

func TestBackend_Delete_EmptyIDsIsNoop(t *testing.T) {
	b, ctx := newBackendForTest(t)
	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	requirepkg.NoError(t, err, "CreateGeneration")
	assertpkg.NoError(t, b.Delete(ctx, gid, nil), "Delete(nil)")
	assertpkg.NoError(t, b.Delete(ctx, gid, []int64{}), "Delete(empty)")
}

func TestBackend_Delete_UnknownGeneration(t *testing.T) {
	b, ctx := newBackendForTest(t)
	err := b.Delete(ctx, vector.GenerationID(9999), []int64{1})
	assertpkg.ErrorIs(t, err, vector.ErrUnknownGeneration)
}

func TestBackend_Stats_CountsCorrectly(t *testing.T) {
	b, ctx := newBackendForTest(t)
	gid := seedAndEmbed(t, b, map[int64][]float32{1: unitVec(768, 0)})

	s, err := b.Stats(ctx, gid)
	requirepkg.NoError(t, err, "Stats")
	assertpkg.Equal(t, int64(1), s.EmbeddingCount)
	assertpkg.Equal(t, int64(0), s.PendingCount)
}

func TestBackend_Stats_PendingCountAfterCreate(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	b, ctx := newBackendForTest(t)
	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(err, "CreateGeneration")
	// CreateGeneration seeds 1 pending row for the one pre-seeded message.
	s, err := b.Stats(ctx, gid)
	require.NoError(err, "Stats")
	assert.Equal(int64(0), s.EmbeddingCount)
	assert.Equal(int64(1), s.PendingCount)
}

func TestBackend_Stats_AggregateAcrossGenerations(t *testing.T) {
	// When gen == 0, Stats returns counts across ALL generations.
	b, ctx := newBackendForTest(t)
	_ = seedAndEmbed(t, b, map[int64][]float32{1: unitVec(768, 0)})

	s, err := b.Stats(ctx, vector.GenerationID(0))
	requirepkg.NoError(t, err, "Stats(0)")
	assertpkg.Equal(t, int64(1), s.EmbeddingCount, "aggregate EmbeddingCount")
}

// TestBackend_Stats_AggregateCountsPerGenerationDuplicates pins the
// fix for the aggregate undercount: when one message exists in both
// the active generation and an in-flight building generation, the
// aggregate path should report two units of embedded work (one per
// generation) rather than collapsing to one via DISTINCT message_id.
func TestBackend_Stats_AggregateCountsPerGenerationDuplicates(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	b, ctx := newBackendForTest(t)

	// First generation: embed message 1, then activate so the next
	// CreateGeneration produces a building gen alongside it instead of
	// reusing the same row.
	genA := seedAndEmbed(t, b, map[int64][]float32{1: unitVec(768, 0)})
	require.NoError(b.ActivateGeneration(ctx, genA), "ActivateGeneration(genA)")

	// Second generation: re-embed the same message 1, mirroring the
	// "rebuild in progress" state where every message is dual-embedded
	// across active + building.
	genB, err := b.CreateGeneration(ctx, "m", 768, "fp-b")
	require.NoError(err, "CreateGeneration(genB)")
	require.NoError(b.Upsert(ctx, genB, []vector.Chunk{
		{MessageID: 1, Vector: unitVec(768, 1)},
	}), "Upsert into genB")

	s, err := b.Stats(ctx, vector.GenerationID(0))
	require.NoError(err, "Stats(0)")
	assert.Equal(int64(2), s.EmbeddingCount, "aggregate EmbeddingCount (one per generation)")

	// Per-generation counts remain semantically "distinct messages in
	// this generation", so each gen still reports 1.
	sa, err := b.Stats(ctx, genA)
	require.NoError(err, "Stats(genA)")
	assert.Equal(int64(1), sa.EmbeddingCount, "genA EmbeddingCount")
	sb, err := b.Stats(ctx, genB)
	require.NoError(err, "Stats(genB)")
	assert.Equal(int64(1), sb.EmbeddingCount, "genB EmbeddingCount")
}

// TestBackend_Upsert_UpdatesMessageCount verifies that
// index_generations.message_count tracks the number of embedded
// messages after both the initial insert and subsequent re-upsert /
// delete. Without this, ActiveGeneration().MessageCount stays at zero
// regardless of how many chunks have been written.
func TestBackend_Upsert_UpdatesMessageCount(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	b, ctx := newBackendForTest(t)

	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(err, "CreateGeneration")

	// Initially zero.
	bg, err := b.BuildingGeneration(ctx)
	require.NoError(err, "BuildingGeneration")
	assert.Equal(int64(0), bg.MessageCount, "initial MessageCount")

	// Upsert three chunks → count 3.
	chunks := []vector.Chunk{
		{MessageID: 1, Vector: unitVec(768, 0), SourceCharLen: 10},
		{MessageID: 2, Vector: unitVec(768, 1), SourceCharLen: 20},
		{MessageID: 3, Vector: unitVec(768, 2), SourceCharLen: 30},
	}
	require.NoError(b.Upsert(ctx, gid, chunks), "Upsert")
	bg, err = b.BuildingGeneration(ctx)
	require.NoError(err, "BuildingGeneration")
	assert.Equal(int64(3), bg.MessageCount, "after initial Upsert")

	// Re-upsert the same messages (update, not insert) → count stays 3.
	require.NoError(b.Upsert(ctx, gid, chunks[:2]), "re-Upsert")
	bg, err = b.BuildingGeneration(ctx)
	require.NoError(err, "BuildingGeneration")
	assert.Equal(int64(3), bg.MessageCount, "after re-Upsert")

	// Delete one → count drops to 2.
	require.NoError(b.Delete(ctx, gid, []int64{2}), "Delete")
	bg, err = b.BuildingGeneration(ctx)
	require.NoError(err, "BuildingGeneration")
	assert.Equal(int64(2), bg.MessageCount, "after Delete")
}

// TestBackend_Stats_UnknownGeneration confirms that passing a non-zero
// generation id that doesn't exist returns an error wrapping
// vector.ErrUnknownGeneration, rather than silently reporting 0 counts
// (which would be indistinguishable from a valid-but-empty generation).
func TestBackend_Stats_UnknownGeneration(t *testing.T) {
	b, ctx := newBackendForTest(t)

	_, err := b.Stats(ctx, vector.GenerationID(9999))
	requirepkg.Error(t, err, "Stats on unknown generation: want error")
	assertpkg.ErrorIs(t, err, vector.ErrUnknownGeneration)
}

func TestBackend_LoadVector(t *testing.T) {
	require := requirepkg.New(t)
	b, ctx := newBackendForTest(t)
	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(err, "CreateGeneration")

	vec := make([]float32, 768)
	for i := range vec {
		vec[i] = float32(i) * 0.01
	}
	chunks := []vector.Chunk{{MessageID: 1, Vector: vec, SourceCharLen: 42}}
	require.NoError(b.Upsert(ctx, gid, chunks), "Upsert")
	require.NoError(b.ActivateGeneration(ctx, gid), "ActivateGeneration")

	got, err := b.LoadVector(ctx, 1)
	require.NoError(err, "LoadVector")
	require.Len(got, 768)
	for i, v := range got {
		require.Equalf(vec[i], v, "mismatch at i=%d", i)
	}
}

func TestBackend_LoadVector_NotEmbedded(t *testing.T) {
	require := requirepkg.New(t)
	b, ctx := newBackendForTest(t)
	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(err, "CreateGeneration")

	vec := make([]float32, 768)
	for i := range vec {
		vec[i] = 0.1
	}
	chunks := []vector.Chunk{{MessageID: 1, Vector: vec, SourceCharLen: 42}}
	require.NoError(b.Upsert(ctx, gid, chunks), "Upsert")
	require.NoError(b.ActivateGeneration(ctx, gid), "ActivateGeneration")

	_, err = b.LoadVector(ctx, 999)
	require.Error(err, "LoadVector for missing message should error")
}

func TestBackend_LoadVector_NoActive(t *testing.T) {
	b, ctx := newBackendForTest(t)
	_, err := b.LoadVector(ctx, 1)
	requirepkg.Error(t, err)
	assertpkg.ErrorIs(t, err, vector.ErrNoActiveGeneration)
}

// TestBackend_Search_ExcludesDedupHidden confirms that Search excludes
// messages hidden by dedup (deleted_at IS NOT NULL), not just those
// deleted from source. Uses a minimal main DB without FTS5.
func TestBackend_Search_ExcludesDedupHidden(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	ctx := context.Background()

	// Minimal main DB: two messages, one dedup-hidden. No FTS5 required.
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(err, "open main")
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`CREATE TABLE messages (
		id INTEGER PRIMARY KEY,
		deleted_at DATETIME,
		deleted_from_source_at DATETIME
	)`)
	require.NoError(err, "create messages")
	_, err = db.Exec(
		`INSERT INTO messages (id, deleted_at) VALUES (1, NULL), (2, '2026-01-01 00:00:00')`)
	require.NoError(err, "insert messages")

	b, err := Open(ctx, Options{
		Path:      t.TempDir() + "/vectors.db",
		Dimension: 768,
		MainDB:    db,
	})
	require.NoError(err, "Open")
	t.Cleanup(func() { _ = b.Close() })

	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(err, "CreateGeneration")
	chunks := []vector.Chunk{
		{MessageID: 1, Vector: unitVec(768, 0)},
		{MessageID: 2, Vector: unitVec(768, 0)},
	}
	require.NoError(b.Upsert(ctx, gid, chunks), "Upsert")

	hits, err := b.Search(ctx, gid, unitVec(768, 0), 10, vector.Filter{})
	require.NoError(err, "Search")
	got := make(map[int64]bool, len(hits))
	for _, h := range hits {
		got[h.MessageID] = true
	}
	assert.True(got[1], "msg 1 missing (live message must appear)")
	assert.False(got[2], "msg 2 (deleted_at IS NOT NULL, must be excluded)")
}

// TestBackend_FilteredMessageIDs_ExcludesDedupHidden confirms that
// filteredMessageIDs excludes messages with deleted_at set.
// Uses a minimal main DB without FTS5.
func TestBackend_FilteredMessageIDs_ExcludesDedupHidden(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	ctx := context.Background()

	// Minimal main DB with source_id for SourceIDs filter.
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(err, "open main")
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`CREATE TABLE messages (
		id INTEGER PRIMARY KEY,
		source_id INTEGER,
		deleted_at DATETIME,
		deleted_from_source_at DATETIME
	)`)
	require.NoError(err, "create messages")
	// Three messages: 1 live, 2 dedup-hidden, 3 source-deleted.
	_, err = db.Exec(`
		INSERT INTO messages (id, source_id, deleted_at, deleted_from_source_at) VALUES
		(1, 1, NULL, NULL),
		(2, 1, '2026-01-01 00:00:00', NULL),
		(3, 1, NULL, '2026-01-01 00:00:00')`)
	require.NoError(err, "insert messages")

	b, err := Open(ctx, Options{
		Path:      t.TempDir() + "/vectors.db",
		Dimension: 768,
		MainDB:    db,
	})
	require.NoError(err, "Open")
	t.Cleanup(func() { _ = b.Close() })

	// Upsert vectors for all three messages directly.
	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(err, "CreateGeneration")
	chunks := []vector.Chunk{
		{MessageID: 1, Vector: unitVec(768, 0)},
		{MessageID: 2, Vector: unitVec(768, 0)},
		{MessageID: 3, Vector: unitVec(768, 0)},
	}
	require.NoError(b.Upsert(ctx, gid, chunks), "Upsert")

	// Filtered search via a non-empty filter triggers filteredMessageIDs.
	hits, err := b.Search(ctx, gid, unitVec(768, 0), 10, vector.Filter{SourceIDs: []int64{1}})
	require.NoError(err, "Search with filter")
	got := make(map[int64]bool, len(hits))
	for _, h := range hits {
		got[h.MessageID] = true
	}
	assert.False(got[2], "msg 2 (deleted_at, must be excluded)")
	assert.False(got[3], "msg 3 (deleted_from_source_at, must be excluded)")
}
