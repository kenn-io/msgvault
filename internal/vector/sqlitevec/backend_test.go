//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/wesm/msgvault/internal/vector"
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
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b, ctx
}

func TestBackend_CreateActivateRetire(t *testing.T) {
	b, ctx := newBackendForTest(t)

	gid, err := b.CreateGeneration(ctx, "nomic-embed-text-v1.5", 768)
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}

	bg, err := b.BuildingGeneration(ctx)
	if err != nil || bg == nil || bg.ID != gid {
		t.Fatalf("BuildingGeneration got (%v, %v), want id=%d", bg, err, gid)
	}
	if _, err := b.ActiveGeneration(ctx); err == nil {
		t.Fatal("ActiveGeneration should error before activation")
	}

	if err := b.ActivateGeneration(ctx, gid); err != nil {
		t.Fatalf("ActivateGeneration: %v", err)
	}
	g, err := b.ActiveGeneration(ctx)
	if err != nil {
		t.Fatalf("ActiveGeneration after activate: %v", err)
	}
	if g.State != vector.GenerationActive {
		t.Errorf("State=%q want active", g.State)
	}
	if g.Fingerprint != "nomic-embed-text-v1.5:768" {
		t.Errorf("Fingerprint=%q", g.Fingerprint)
	}

	if err := b.RetireGeneration(ctx, gid); err != nil {
		t.Fatalf("RetireGeneration: %v", err)
	}
	if _, err := b.ActiveGeneration(ctx); err == nil {
		t.Fatal("ActiveGeneration should error after retire")
	}
}

func TestBackend_CreateGeneration_SeedsPending(t *testing.T) {
	b, ctx := newBackendForTest(t)
	gid, err := b.CreateGeneration(ctx, "m", 768)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var n int
	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ?`, gid,
	).Scan(&n); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if n != 1 {
		t.Errorf("pending count = %d, want 1", n)
	}
}

// TestBackend_CreateGeneration_ResumesBuilding confirms that calling
// CreateGeneration while a building row already exists with the same
// fingerprint returns the existing id instead of failing on the unique
// index. This makes retries after a crash idempotent.
func TestBackend_CreateGeneration_ResumesBuilding(t *testing.T) {
	b, ctx := newBackendForTest(t)

	first, err := b.CreateGeneration(ctx, "m", 768)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}

	second, err := b.CreateGeneration(ctx, "m", 768)
	if err != nil {
		t.Fatalf("second Create with matching fingerprint: %v", err)
	}
	if first != second {
		t.Errorf("Create returned new id %d, want reused %d", second, first)
	}
}

// TestBackend_CreateGeneration_MismatchedFingerprint checks that a
// second CreateGeneration call with a different fingerprint while
// another build is in progress surfaces an actionable error wrapping
// vector.ErrBuildingInProgress, instead of a raw SQLite uniqueness
// error.
func TestBackend_CreateGeneration_MismatchedFingerprint(t *testing.T) {
	b, ctx := newBackendForTest(t)

	if _, err := b.CreateGeneration(ctx, "model-a", 768); err != nil {
		t.Fatalf("first Create: %v", err)
	}

	_, err := b.CreateGeneration(ctx, "model-b", 768)
	if err == nil {
		t.Fatal("second Create with different fingerprint: want error, got nil")
	}
	if !errors.Is(err, vector.ErrBuildingInProgress) {
		t.Errorf("error = %v, want wrapping ErrBuildingInProgress", err)
	}
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
	b, ctx := newBackendForTest(t)

	gen, err := b.CreateGeneration(ctx, "m", 768)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}

	// Simulate Queue.Complete: remove the pending row for the only
	// pre-seeded message (id=1) as if it were already embedded.
	if _, err := b.db.ExecContext(ctx,
		`DELETE FROM pending_embeddings WHERE generation_id = ? AND message_id = ?`,
		int64(gen), int64(1)); err != nil {
		t.Fatalf("delete pending: %v", err)
	}

	// Resume: CreateGeneration must reuse the existing building gen
	// and NOT re-enqueue the completed message.
	resumed, err := b.CreateGeneration(ctx, "m", 768)
	if err != nil {
		t.Fatalf("resume Create: %v", err)
	}
	if resumed != gen {
		t.Errorf("resumed gen = %d, want reused %d", resumed, gen)
	}
	var pending int
	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ? AND message_id = 1`,
		int64(gen)).Scan(&pending); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending != 0 {
		t.Errorf("pending count for completed msg 1 = %d, want 0 (resume re-seeded a completed message)", pending)
	}
}

// TestBackend_CreateGeneration_ResumeReseedsUnseededGeneration covers
// the "crash between row insert and seed commit" path: a building row
// exists but seeded_at is NULL because the previous attempt died
// before the seed transaction committed. A naive resume would skip
// seedPending, leave pending_embeddings empty, and let
// `msgvault build-embeddings` activate the unseeded generation — silently
// replacing the prior active index with an empty one. The fix is to
// re-run seedPending whenever seeded_at IS NULL on resume.
func TestBackend_CreateGeneration_ResumeReseedsUnseededGeneration(t *testing.T) {
	b, ctx := newBackendForTest(t)

	gen, err := b.CreateGeneration(ctx, "m", 768)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	// Simulate the crash window: clear seeded_at AND wipe the seeded
	// rows so the post-resume pending count is exactly what the resume
	// re-seed would produce. Without this we couldn't distinguish
	// "rows are present because resume re-seeded" from "rows are
	// present because the original seed left them there".
	if _, err := b.db.ExecContext(ctx,
		`UPDATE index_generations SET seeded_at = NULL WHERE id = ?`, int64(gen)); err != nil {
		t.Fatalf("clear seeded_at: %v", err)
	}
	if _, err := b.db.ExecContext(ctx,
		`DELETE FROM pending_embeddings WHERE generation_id = ?`, int64(gen)); err != nil {
		t.Fatalf("clear pending: %v", err)
	}

	resumed, err := b.CreateGeneration(ctx, "m", 768)
	if err != nil {
		t.Fatalf("resume Create: %v", err)
	}
	if resumed != gen {
		t.Errorf("resumed gen = %d, want reused %d", resumed, gen)
	}
	var pending int
	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ?`,
		int64(gen)).Scan(&pending); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending != 1 {
		t.Errorf("pending count after resume = %d, want 1 (resume must re-seed an unseeded build)", pending)
	}
	// And seeded_at should now be populated so a second resume
	// would correctly skip re-seeding.
	var seededAt sql.NullInt64
	if err := b.db.QueryRowContext(ctx,
		`SELECT seeded_at FROM index_generations WHERE id = ?`, int64(gen)).Scan(&seededAt); err != nil {
		t.Fatalf("read seeded_at: %v", err)
	}
	if !seededAt.Valid {
		t.Error("seeded_at still NULL after resume re-seed; second resume would re-seed again")
	}
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
	b, ctx := newBackendForTest(t)

	gen1, isNew1, err := b.claimOrInsertBuilding(ctx, "m", 768, "m:768", time.Now().Unix())
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if !isNew1 {
		t.Errorf("first claim: isNew=false, want true")
	}

	// Second claim must reuse the row (isNew=false), and the path
	// would have hit the unique constraint had we tried INSERT first
	// without the SELECT. The recovery branch is what guarantees we
	// don't surface a raw SQLite error if some other writer wins.
	gen2, isNew2, err := b.claimOrInsertBuilding(ctx, "m", 768, "m:768", time.Now().Unix())
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if isNew2 {
		t.Errorf("second claim: isNew=true, want false (existing row should be reused)")
	}
	if gen1 != gen2 {
		t.Errorf("second claim: gen=%d, want reused %d", gen2, gen1)
	}
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
	ctx := context.Background()

	// Build a backend whose main DB has many messages so seedPending
	// has enough work that we can race a visibility poll against it.
	// 5_000 rows is comfortably more than the one row in the standard
	// helper and drives seedPending into the millisecond range even on
	// a fast laptop — far longer than the polling interval below.
	main := openMainDBWithOneMessage(t)
	insert, err := main.PrepareContext(ctx, `INSERT INTO messages (id) VALUES (?)`)
	if err != nil {
		t.Fatalf("prepare insert: %v", err)
	}
	defer func() { _ = insert.Close() }()
	for i := int64(2); i <= 5000; i++ {
		if _, err := insert.ExecContext(ctx, i); err != nil {
			t.Fatalf("insert msg %d: %v", i, err)
		}
	}

	b, err := Open(ctx, Options{
		Path:      filepath.Join(t.TempDir(), "vectors.db"),
		Dimension: 768,
		MainDB:    main,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	done := make(chan error, 1)
	go func() {
		_, err := b.CreateGeneration(ctx, "m", 768)
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

	if err := <-done; err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}
	if doneFiredFirst {
		t.Fatal("CreateGeneration returned before the building row became visible — commit was deferred to after seed")
	}
	if !visibleInFlight {
		t.Fatal("building generation was never visible while CreateGeneration was in flight")
	}
}

func TestBackend_CreateGeneration_SkipsDeletedMessages(t *testing.T) {
	b := openBackendWithOneDeletedMessage(t)
	t.Cleanup(func() { _ = b.Close() })
	ctx := context.Background()
	gid, err := b.CreateGeneration(ctx, "m", 768)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var n int
	_ = b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ?`, gid).Scan(&n)
	if n != 0 {
		t.Errorf("pending count for deleted message = %d, want 0", n)
	}
}

// TestBackend_SeedPending_SkipsDedupHidden verifies that seedPending
// omits messages soft-deleted by dedup (deleted_at IS NOT NULL).
func TestBackend_SeedPending_SkipsDedupHidden(t *testing.T) {
	t.Helper()
	ctx := context.Background()

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open main: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE messages (
		id INTEGER PRIMARY KEY,
		deleted_at DATETIME,
		deleted_from_source_at DATETIME
	)`); err != nil {
		t.Fatalf("create messages: %v", err)
	}
	// Insert one live and one dedup-hidden message.
	if _, err := db.Exec(`INSERT INTO messages (id) VALUES (1)`); err != nil {
		t.Fatalf("insert live: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO messages (id, deleted_at) VALUES (2, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("insert dedup-hidden: %v", err)
	}

	b, err := Open(ctx, Options{
		Path:      t.TempDir() + "/vectors.db",
		Dimension: 768,
		MainDB:    db,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	gid, err := b.CreateGeneration(ctx, "m", 768)
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}
	var n int
	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ?`, gid).Scan(&n); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if n != 1 {
		t.Errorf("pending count = %d, want 1 (dedup-hidden message must be excluded)", n)
	}
}

// TestBackend_Upsert_WritesEmbeddingAndVector verifies Upsert's
// contract: it writes the embeddings row and the dimension-specific
// vec0 row, and explicitly does NOT touch pending_embeddings. The
// queue is the sole owner of that table so that Queue.Complete's
// token check can prevent a stale worker from wiping a newer worker's
// claim.
func TestBackend_Upsert_WritesEmbeddingAndVector(t *testing.T) {
	b, ctx := newBackendForTest(t)
	gid, err := b.CreateGeneration(ctx, "m", 768)
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}

	vec := make([]float32, 768)
	for i := range vec {
		vec[i] = 0.1
	}
	chunks := []vector.Chunk{{MessageID: 1, Vector: vec, SourceCharLen: 42}}
	if err := b.Upsert(ctx, gid, chunks); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	var n int
	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = ? AND message_id = 1`, gid).Scan(&n); err != nil {
		t.Fatalf("count embeddings: %v", err)
	}
	if n != 1 {
		t.Errorf("embeddings count = %d, want 1", n)
	}

	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM vectors_vec_d768 WHERE generation_id = ? AND message_id = 1`, gid).Scan(&n); err != nil {
		t.Fatalf("count vectors_vec_d768: %v", err)
	}
	if n != 1 {
		t.Errorf("vectors_vec_d768 count = %d, want 1", n)
	}

	// Pending row is still present — the queue owns that table and
	// only Queue.Complete may remove it.
	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ? AND message_id = 1`, gid).Scan(&n); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if n != 1 {
		t.Errorf("pending count = %d, want 1 (Upsert must not touch pending_embeddings)", n)
	}
}

func TestBackend_Upsert_DimensionMismatch(t *testing.T) {
	b, ctx := newBackendForTest(t)
	gid, err := b.CreateGeneration(ctx, "m", 768)
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}

	short := make([]float32, 64) // wrong dim
	err = b.Upsert(ctx, gid, []vector.Chunk{{MessageID: 1, Vector: short}})
	if !errors.Is(err, vector.ErrDimensionMismatch) {
		t.Errorf("err = %v, want ErrDimensionMismatch", err)
	}
}

func TestBackend_Upsert_EmptyChunks(t *testing.T) {
	b, ctx := newBackendForTest(t)
	gid, err := b.CreateGeneration(ctx, "m", 768)
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}

	if err := b.Upsert(ctx, gid, nil); err != nil {
		t.Fatalf("Upsert(nil): %v", err)
	}
	if err := b.Upsert(ctx, gid, []vector.Chunk{}); err != nil {
		t.Fatalf("Upsert(empty): %v", err)
	}

	var n int
	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = ?`, gid).Scan(&n); err != nil {
		t.Fatalf("count embeddings: %v", err)
	}
	if n != 0 {
		t.Errorf("embeddings count = %d, want 0", n)
	}
}

func TestBackend_Upsert_UnknownGeneration(t *testing.T) {
	b, ctx := newBackendForTest(t)

	vec := make([]float32, 768)
	err := b.Upsert(ctx, vector.GenerationID(9999), []vector.Chunk{{MessageID: 1, Vector: vec}})
	if !errors.Is(err, vector.ErrUnknownGeneration) {
		t.Errorf("err = %v, want ErrUnknownGeneration", err)
	}
}

func TestBackend_Upsert_MultiChunkAndTruncated(t *testing.T) {
	b, ctx := newBackendForTest(t)
	gid, err := b.CreateGeneration(ctx, "m", 768)
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}

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
	if err := b.Upsert(ctx, gid, chunks); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	var n int
	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = ?`, gid).Scan(&n); err != nil {
		t.Fatalf("count embeddings: %v", err)
	}
	if n != 2 {
		t.Errorf("embeddings count = %d, want 2", n)
	}

	var trunc int
	if err := b.db.QueryRowContext(ctx,
		`SELECT truncated FROM embeddings WHERE generation_id = ? AND message_id = 1`, gid).Scan(&trunc); err != nil {
		t.Fatalf("scan truncated msg 1: %v", err)
	}
	if trunc != 1 {
		t.Errorf("truncated for msg 1 = %d, want 1", trunc)
	}
	if err := b.db.QueryRowContext(ctx,
		`SELECT truncated FROM embeddings WHERE generation_id = ? AND message_id = 2`, gid).Scan(&trunc); err != nil {
		t.Fatalf("scan truncated msg 2: %v", err)
	}
	if trunc != 0 {
		t.Errorf("truncated for msg 2 = %d, want 0", trunc)
	}

	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM vectors_vec_d768 WHERE generation_id = ?`, gid).Scan(&n); err != nil {
		t.Fatalf("count vectors_vec_d768: %v", err)
	}
	if n != 2 {
		t.Errorf("vectors_vec_d768 count = %d, want 2", n)
	}
}

func TestBackend_Upsert_ReplacesExisting(t *testing.T) {
	b, ctx := newBackendForTest(t)
	gid, err := b.CreateGeneration(ctx, "m", 768)
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}

	vec1 := make([]float32, 768)
	for i := range vec1 {
		vec1[i] = 0.1
	}
	if err := b.Upsert(ctx, gid, []vector.Chunk{{MessageID: 1, Vector: vec1, SourceCharLen: 10}}); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}

	vec2 := make([]float32, 768)
	for i := range vec2 {
		vec2[i] = 0.9
	}
	if err := b.Upsert(ctx, gid, []vector.Chunk{{MessageID: 1, Vector: vec2, SourceCharLen: 999}}); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}

	var n int
	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = ? AND message_id = 1`, gid).Scan(&n); err != nil {
		t.Fatalf("count embeddings: %v", err)
	}
	if n != 1 {
		t.Errorf("embeddings count = %d, want 1", n)
	}

	var charLen int
	if err := b.db.QueryRowContext(ctx,
		`SELECT source_char_len FROM embeddings WHERE generation_id = ? AND message_id = 1`, gid).Scan(&charLen); err != nil {
		t.Fatalf("scan source_char_len: %v", err)
	}
	if charLen != 999 {
		t.Errorf("source_char_len = %d, want 999", charLen)
	}

	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM vectors_vec_d768 WHERE generation_id = ? AND message_id = 1`, gid).Scan(&n); err != nil {
		t.Fatalf("count vectors_vec_d768: %v", err)
	}
	if n != 1 {
		t.Errorf("vectors_vec_d768 count = %d, want 1", n)
	}
}

func TestBackend_Search_ReturnsRankedHits(t *testing.T) {
	b, ctx := newBackendForTest(t)
	gid := seedAndEmbed(t, b, map[int64][]float32{
		10: unitVec(768, 0),
		11: unitVec(768, 1),
		12: unitVec(768, 2),
	})

	hits, err := b.Search(ctx, gid, unitVec(768, 1), 2, vector.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2", len(hits))
	}
	if hits[0].MessageID != 11 {
		t.Errorf("top hit = %d, want 11", hits[0].MessageID)
	}
	if hits[0].Rank != 1 {
		t.Errorf("top rank = %d, want 1", hits[0].Rank)
	}
}

func TestBackend_Search_EmptyQueryVector(t *testing.T) {
	b, ctx := newBackendForTest(t)
	gid, err := b.CreateGeneration(ctx, "m", 768)
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}
	_, err = b.Search(ctx, gid, nil, 5, vector.Filter{})
	if err == nil {
		t.Fatal("Search with nil queryVec should error")
	}
	_, err = b.Search(ctx, gid, []float32{}, 5, vector.Filter{})
	if err == nil {
		t.Fatal("Search with empty queryVec should error")
	}
}

func TestBackend_Search_UnknownGeneration(t *testing.T) {
	b, ctx := newBackendForTest(t)
	vec := unitVec(768, 0)
	_, err := b.Search(ctx, vector.GenerationID(9999), vec, 5, vector.Filter{})
	if !errors.Is(err, vector.ErrUnknownGeneration) {
		t.Errorf("err = %v, want ErrUnknownGeneration", err)
	}
}

func TestBackend_Search_DimensionMismatch(t *testing.T) {
	b, ctx := newBackendForTest(t)
	gid, err := b.CreateGeneration(ctx, "m", 768)
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}
	_, err = b.Search(ctx, gid, unitVec(64, 0), 5, vector.Filter{})
	if !errors.Is(err, vector.ErrDimensionMismatch) {
		t.Errorf("err = %v, want ErrDimensionMismatch", err)
	}
}

// TestBackend_Search_FilterIDsExceedSQLiteParamCap exercises the
// json_each path in resolveFilter with a filter that resolves to more
// messages than SQLite's ~999 practical bound-parameter cap. The old
// implementation expanded the id set into one `IN (?,?,...)` list per
// id and failed with `too many SQL variables` once it crossed the cap.
func TestBackend_Search_FilterIDsExceedSQLiteParamCap(t *testing.T) {
	b, ctx, _ := newFusedBackendForTest(t)

	const total = 1200 // well past SQLite's 999-variable ceiling
	// The helper seeds 3 FTS rows; insert `total` more messages each
	// with a `from` recipient row pointing at the same participant so
	// a single sender filter matches all of them.
	if _, err := b.mainDB.ExecContext(ctx,
		`DELETE FROM messages; DELETE FROM messages_fts; DELETE FROM message_recipients`); err != nil {
		t.Fatalf("reset main: %v", err)
	}
	insertMsg, err := b.mainDB.PrepareContext(ctx,
		`INSERT INTO messages (id) VALUES (?)`)
	if err != nil {
		t.Fatalf("prepare msg: %v", err)
	}
	defer func() { _ = insertMsg.Close() }()
	insertMR, err := b.mainDB.PrepareContext(ctx,
		`INSERT INTO message_recipients (message_id, recipient_type, participant_id) VALUES (?, 'from', 42)`)
	if err != nil {
		t.Fatalf("prepare mr: %v", err)
	}
	defer func() { _ = insertMR.Close() }()
	vecs := make(map[int64][]float32, total)
	for i := int64(1); i <= total; i++ {
		if _, err := insertMsg.ExecContext(ctx, i); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
		if _, err := insertMR.ExecContext(ctx, i); err != nil {
			t.Fatalf("insert mr %d: %v", i, err)
		}
		vecs[i] = unitVec(768, 0)
	}

	gid, err := b.CreateGeneration(ctx, "m", 768)
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}
	// Upsert a few chunks so Search has something to rank. We don't
	// need all `total` embedded — the filter is what we're stressing.
	chunks := make([]vector.Chunk, 0, 5)
	for i := int64(1); i <= 5; i++ {
		chunks = append(chunks, vector.Chunk{MessageID: i, Vector: vecs[i]})
	}
	if err := b.Upsert(ctx, gid, chunks); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	hits, err := b.Search(ctx, gid, unitVec(768, 0), 3, vector.Filter{SenderGroups: [][]int64{{42}}})
	if err != nil {
		t.Fatalf("Search with broad filter (%d ids) failed: %v", total, err)
	}
	if len(hits) == 0 {
		t.Errorf("expected at least one hit after filter, got 0")
	}
}

// TestBackend_Search_NewFilterFields exercises the filter fields added
// to match the existing SQLite search surface: to/cc/bcc recipients,
// larger/smaller size bounds, and subject substring match.
func TestBackend_Search_NewFilterFields(t *testing.T) {
	b, ctx, _ := newFusedBackendForTest(t)

	// Reset and seed 4 messages with distinct recipient / size / subject
	// profiles so each assertion is unambiguous.
	if _, err := b.mainDB.ExecContext(ctx,
		`DELETE FROM messages; DELETE FROM messages_fts; DELETE FROM message_recipients`); err != nil {
		t.Fatalf("reset: %v", err)
	}

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
		if _, err := b.mainDB.ExecContext(ctx,
			`INSERT INTO messages (id, subject, size_estimate) VALUES (?, ?, ?)`,
			r.id, r.subject, r.size); err != nil {
			t.Fatalf("insert msg %d: %v", r.id, err)
		}
		if r.to != 0 {
			if _, err := b.mainDB.ExecContext(ctx,
				`INSERT INTO message_recipients (message_id, recipient_type, participant_id)
				 VALUES (?, 'to', ?)`, r.id, r.to); err != nil {
				t.Fatalf("insert to: %v", err)
			}
		}
		if r.cc != 0 {
			if _, err := b.mainDB.ExecContext(ctx,
				`INSERT INTO message_recipients (message_id, recipient_type, participant_id)
				 VALUES (?, 'cc', ?)`, r.id, r.cc); err != nil {
				t.Fatalf("insert cc: %v", err)
			}
		}
	}

	gid, err := b.CreateGeneration(ctx, "m", 768)
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}
	chunks := make([]vector.Chunk, 0, len(rows))
	for _, r := range rows {
		chunks = append(chunks, vector.Chunk{MessageID: r.id, Vector: unitVec(768, 0)})
	}
	if err := b.Upsert(ctx, gid, chunks); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	matched := func(t *testing.T, f vector.Filter) map[int64]bool {
		t.Helper()
		hits, err := b.Search(ctx, gid, unitVec(768, 0), 10, f)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		got := make(map[int64]bool, len(hits))
		for _, h := range hits {
			got[h.MessageID] = true
		}
		return got
	}

	t.Run("ToGroups_singleGroup", func(t *testing.T) {
		got := matched(t, vector.Filter{ToGroups: [][]int64{{20}}})
		if !got[2] || !got[3] || got[1] || got[4] {
			t.Errorf("ToGroups=[[20]]: got %v, want {2,3}", got)
		}
	})
	t.Run("CcGroups_singleGroup", func(t *testing.T) {
		got := matched(t, vector.Filter{CcGroups: [][]int64{{10}}})
		if !got[2] || got[1] || got[3] || got[4] {
			t.Errorf("CcGroups=[[10]]: got %v, want {2}", got)
		}
	})
	t.Run("LargerThan", func(t *testing.T) {
		size := int64(1_000_000)
		got := matched(t, vector.Filter{LargerThan: &size})
		if !got[2] || !got[4] || got[1] || got[3] {
			t.Errorf("LargerThan=1MB: got %v, want {2,4}", got)
		}
	})
	t.Run("SmallerThan", func(t *testing.T) {
		size := int64(1_000_000)
		got := matched(t, vector.Filter{SmallerThan: &size})
		if !got[1] || !got[3] || got[2] || got[4] {
			t.Errorf("SmallerThan=1MB: got %v, want {1,3}", got)
		}
	})
	t.Run("SubjectSubstring", func(t *testing.T) {
		got := matched(t, vector.Filter{SubjectSubstrings: []string{"quarterly"}})
		if !got[1] || !got[2] || !got[4] || got[3] {
			t.Errorf("subject=quarterly: got %v, want {1,2,4}", got)
		}
	})
	t.Run("MultipleSubjectsANDed", func(t *testing.T) {
		got := matched(t, vector.Filter{SubjectSubstrings: []string{"quarterly", "deep"}})
		if !got[4] || got[1] || got[2] || got[3] {
			t.Errorf("subject=[quarterly, deep]: got %v, want {4}", got)
		}
	})
	t.Run("CombinedFilter", func(t *testing.T) {
		size := int64(1_000_000)
		got := matched(t, vector.Filter{
			ToGroups:          [][]int64{{20}},
			LargerThan:        &size,
			SubjectSubstrings: []string{"quarterly"},
		})
		if !got[2] || got[1] || got[3] || got[4] {
			t.Errorf("combined to=20 + >1MB + quarterly: got %v, want {2}", got)
		}
	})
}

// TestBackend_Search_RecipientGroupsAreANDed asserts that multiple
// groups for the same recipient field require the message to match
// EVERY group — i.e. `to:alice to:bob` is NOT the same as
// `to:(alice OR bob)`. Each group becomes its own EXISTS clause and
// they are AND'd together. Same shape as label group AND'ing.
func TestBackend_Search_RecipientGroupsAreANDed(t *testing.T) {
	b, ctx, _ := newFusedBackendForTest(t)

	if _, err := b.mainDB.ExecContext(ctx,
		`DELETE FROM messages; DELETE FROM messages_fts; DELETE FROM message_recipients; DELETE FROM message_labels`); err != nil {
		t.Fatalf("reset: %v", err)
	}

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
		if _, err := b.mainDB.ExecContext(ctx,
			`INSERT INTO messages (id) VALUES (?)`, r.id); err != nil {
			t.Fatalf("insert msg %d: %v", r.id, err)
		}
		for _, p := range r.tos {
			if _, err := b.mainDB.ExecContext(ctx,
				`INSERT INTO message_recipients (message_id, recipient_type, participant_id)
				 VALUES (?, 'to', ?)`, r.id, p); err != nil {
				t.Fatalf("insert to: %v", err)
			}
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
		if _, err := b.mainDB.ExecContext(ctx,
			`INSERT INTO message_labels (message_id, label_id) VALUES (?, ?)`,
			ml.mid, ml.lid); err != nil {
			t.Fatalf("insert message_label: %v", err)
		}
	}

	gid, err := b.CreateGeneration(ctx, "m", 768)
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}
	chunks := make([]vector.Chunk, 0, len(rows))
	for _, r := range rows {
		chunks = append(chunks, vector.Chunk{MessageID: r.id, Vector: unitVec(768, 0)})
	}
	if err := b.Upsert(ctx, gid, chunks); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	matched := func(t *testing.T, f vector.Filter) map[int64]bool {
		t.Helper()
		hits, err := b.Search(ctx, gid, unitVec(768, 0), 10, f)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		got := make(map[int64]bool, len(hits))
		for _, h := range hits {
			got[h.MessageID] = true
		}
		return got
	}

	t.Run("two_to_groups_require_both", func(t *testing.T) {
		// `to:100 to:200` ⇒ ToGroups=[[100],[200]]; only msg 2 has both.
		got := matched(t, vector.Filter{ToGroups: [][]int64{{100}, {200}}})
		if !got[2] || got[1] || got[3] {
			t.Errorf("ToGroups=[[100],[200]]: got %v, want only {2}", got)
		}
	})

	t.Run("two_label_groups_require_both", func(t *testing.T) {
		// `label:1 label:2` ⇒ LabelGroups=[[1],[2]]; only msg 2 has both.
		got := matched(t, vector.Filter{LabelGroups: [][]int64{{1}, {2}}})
		if !got[2] || got[1] || got[3] {
			t.Errorf("LabelGroups=[[1],[2]]: got %v, want only {2}", got)
		}
	})

	t.Run("OR_within_a_group_still_works", func(t *testing.T) {
		// One group containing both ids ⇒ matches messages with either.
		got := matched(t, vector.Filter{ToGroups: [][]int64{{100, 200}}})
		if !got[1] || !got[2] || !got[3] {
			t.Errorf("ToGroups=[[100,200]]: got %v, want {1,2,3}", got)
		}
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
	b, ctx, _ := newFusedBackendForTest(t)

	// Reset the fused helper's seed data so we control the rows.
	if _, err := b.mainDB.ExecContext(ctx,
		`DELETE FROM messages; DELETE FROM messages_fts; DELETE FROM message_recipients`); err != nil {
		t.Fatalf("reset main: %v", err)
	}

	// msg 1: sender_id=100, NO `from` recipient row → must NOT match.
	if _, err := b.mainDB.ExecContext(ctx,
		`INSERT INTO messages (id, sender_id) VALUES (1, 100)`); err != nil {
		t.Fatalf("insert msg 1: %v", err)
	}
	// msg 2: no sender_id, `from` recipient row with pid=100 → matches.
	if _, err := b.mainDB.ExecContext(ctx,
		`INSERT INTO messages (id) VALUES (2)`); err != nil {
		t.Fatalf("insert msg 2: %v", err)
	}
	if _, err := b.mainDB.ExecContext(ctx,
		`INSERT INTO message_recipients (message_id, recipient_type, participant_id)
		 VALUES (2, 'from', 100)`); err != nil {
		t.Fatalf("insert mr: %v", err)
	}
	// msg 3: different sender (`from` row for pid=999) → must NOT match.
	if _, err := b.mainDB.ExecContext(ctx,
		`INSERT INTO messages (id) VALUES (3)`); err != nil {
		t.Fatalf("insert msg 3: %v", err)
	}
	if _, err := b.mainDB.ExecContext(ctx,
		`INSERT INTO message_recipients (message_id, recipient_type, participant_id)
		 VALUES (3, 'from', 999)`); err != nil {
		t.Fatalf("insert mr 3: %v", err)
	}

	gid, err := b.CreateGeneration(ctx, "m", 768)
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}
	chunks := []vector.Chunk{
		{MessageID: 1, Vector: unitVec(768, 0)},
		{MessageID: 2, Vector: unitVec(768, 0)},
		{MessageID: 3, Vector: unitVec(768, 0)},
	}
	if err := b.Upsert(ctx, gid, chunks); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	hits, err := b.Search(ctx, gid, unitVec(768, 0), 10, vector.Filter{SenderGroups: [][]int64{{100}}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := make(map[int64]bool)
	for _, h := range hits {
		got[h.MessageID] = true
	}
	if got[1] {
		t.Errorf("unexpected hit: msg 1 (sender_id=100 without `from` recipient row must not match — sender filter uses recipient rows only)")
	}
	if !got[2] {
		t.Errorf("hit missing: msg 2 (`from` recipient row pid=100)")
	}
	if got[3] {
		t.Errorf("unexpected hit: msg 3 (different `from` recipient)")
	}
}

// TestBackend_Search_SenderGroupsAreANDed_AtMessageLevel asserts that
// repeated `from:` operators are AND'd at the message level — a
// message with two `from` recipient rows can satisfy two `from:`
// tokens even though messages.sender_id is single-valued. This
// matches internal/store/api.go's behavior for repeated `from:` and
// regression-guards the bug where SenderGroups were collapsed to a
// participant-level intersection (which would drop such messages).
func TestBackend_Search_SenderGroupsAreANDed_AtMessageLevel(t *testing.T) {
	b, ctx, _ := newFusedBackendForTest(t)

	if _, err := b.mainDB.ExecContext(ctx,
		`DELETE FROM messages; DELETE FROM messages_fts; DELETE FROM message_recipients`); err != nil {
		t.Fatalf("reset: %v", err)
	}

	// Three messages, each seeded with explicit `from` recipient rows.
	// Sender-group filtering resolves against those rows only (matching
	// the SQLite FTS path), so `from:100 from:200` requires two
	// distinct `from` rows on the same message.
	//   1: `from` rows {100}           — matches group [100] only
	//   2: `from` rows {100, 200}      — matches both groups
	//   3: `from` rows {100, 200}      — matches both groups
	if _, err := b.mainDB.ExecContext(ctx,
		`INSERT INTO messages (id) VALUES (1), (2), (3)`); err != nil {
		t.Fatalf("insert messages: %v", err)
	}
	for _, mr := range []struct {
		mid int64
		pid int64
	}{
		{1, 100},
		{2, 100}, {2, 200},
		{3, 100}, {3, 200},
	} {
		if _, err := b.mainDB.ExecContext(ctx,
			`INSERT INTO message_recipients (message_id, recipient_type, participant_id)
			 VALUES (?, 'from', ?)`, mr.mid, mr.pid); err != nil {
			t.Fatalf("insert mr: %v", err)
		}
	}

	gid, err := b.CreateGeneration(ctx, "m", 768)
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}
	chunks := []vector.Chunk{
		{MessageID: 1, Vector: unitVec(768, 0)},
		{MessageID: 2, Vector: unitVec(768, 0)},
		{MessageID: 3, Vector: unitVec(768, 0)},
	}
	if err := b.Upsert(ctx, gid, chunks); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	matched := func(t *testing.T, f vector.Filter) map[int64]bool {
		t.Helper()
		hits, err := b.Search(ctx, gid, unitVec(768, 0), 10, f)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		got := make(map[int64]bool, len(hits))
		for _, h := range hits {
			got[h.MessageID] = true
		}
		return got
	}

	t.Run("two_groups_AND_at_message_level", func(t *testing.T) {
		// `from:100 from:200` ⇒ SenderGroups=[[100],[200]]:
		//   msg 1: `from` rows {100} — no 200 row → drop
		//   msg 2: `from` rows {100, 200} → keep
		//   msg 3: `from` rows {100, 200} → keep
		got := matched(t, vector.Filter{SenderGroups: [][]int64{{100}, {200}}})
		if got[1] || !got[2] || !got[3] {
			t.Errorf("SenderGroups=[[100],[200]]: got %v, want {2,3}", got)
		}
	})

	t.Run("single_group_OR_within", func(t *testing.T) {
		// One group containing both ids ⇒ matches messages with any
		// `from` row referencing either id (OR within group).
		got := matched(t, vector.Filter{SenderGroups: [][]int64{{100, 200}}})
		if !got[1] || !got[2] || !got[3] {
			t.Errorf("SenderGroups=[[100,200]]: got %v, want {1,2,3}", got)
		}
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
	b, ctx, _ := newFusedBackendForTest(t)

	if _, err := b.mainDB.ExecContext(ctx,
		`DELETE FROM messages; DELETE FROM messages_fts; DELETE FROM message_recipients`); err != nil {
		t.Fatalf("reset: %v", err)
	}

	// Two messages: 1 live, 2 soft-deleted.
	if _, err := b.mainDB.ExecContext(ctx,
		`INSERT INTO messages (id, deleted_from_source_at) VALUES (1, NULL), (2, '2026-01-01 00:00:00')`); err != nil {
		t.Fatalf("insert messages: %v", err)
	}

	gid, err := b.CreateGeneration(ctx, "m", 768)
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}
	chunks := []vector.Chunk{
		{MessageID: 1, Vector: unitVec(768, 0)},
		{MessageID: 2, Vector: unitVec(768, 0)},
	}
	if err := b.Upsert(ctx, gid, chunks); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Empty filter: must still exclude the soft-deleted message.
	hits, err := b.Search(ctx, gid, unitVec(768, 0), 10, vector.Filter{})
	if err != nil {
		t.Fatalf("Search (empty filter): %v", err)
	}
	got := make(map[int64]bool, len(hits))
	for _, h := range hits {
		got[h.MessageID] = true
	}
	if !got[1] {
		t.Errorf("hit missing: msg 1 (not deleted, must appear)")
	}
	if got[2] {
		t.Errorf("hit present: msg 2 (deleted_from_source_at IS NOT NULL, must be excluded)")
	}
}

// TestBackend_Search_OverFetchesToHonorKWhenTopHitsDeleted regresses
// the case where soft-deleted messages occupy slots in the top-k of
// the raw ANN result. Post-filtering deletions after fetching exactly
// k hits shrank the returned slice below k even when plenty more live
// neighbors existed just below the cutoff. The fast path must
// over-fetch enough to still return k live hits in this situation.
func TestBackend_Search_OverFetchesToHonorKWhenTopHitsDeleted(t *testing.T) {
	b, ctx, _ := newFusedBackendForTest(t)

	if _, err := b.mainDB.ExecContext(ctx,
		`DELETE FROM messages; DELETE FROM messages_fts; DELETE FROM message_recipients`); err != nil {
		t.Fatalf("reset: %v", err)
	}

	// Seed 8 messages: 1–3 are soft-deleted and embedded at the exact
	// query vector (distance 0), 4–8 are live and embedded at
	// successively more distant perturbations. With k=5 and the old
	// "fetch k, post-filter" strategy, sqlite-vec's top-5 would be
	// {1,2,3,4,5}; dropping the deleted rows left only {4,5}. The
	// over-fetch fix should now return 5 live hits.
	if _, err := b.mainDB.ExecContext(ctx, `
		INSERT INTO messages (id, deleted_from_source_at) VALUES
		    (1, '2026-01-01'), (2, '2026-01-01'), (3, '2026-01-01'),
		    (4, NULL), (5, NULL), (6, NULL), (7, NULL), (8, NULL)`); err != nil {
		t.Fatalf("insert messages: %v", err)
	}

	gid, err := b.CreateGeneration(ctx, "m", 768)
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}

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
	if err := b.Upsert(ctx, gid, chunks); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	hits, err := b.Search(ctx, gid, unitVec(768, 0), 5, vector.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 5 {
		t.Fatalf("len(hits) = %d, want 5 (over-fetch must absorb deletions)", len(hits))
	}
	got := make(map[int64]bool, len(hits))
	for _, h := range hits {
		got[h.MessageID] = true
	}
	for _, deleted := range []int64{1, 2, 3} {
		if got[deleted] {
			t.Errorf("hits contain deleted msg %d", deleted)
		}
	}
	for _, live := range []int64{4, 5, 6, 7, 8} {
		if !got[live] {
			t.Errorf("hits missing live msg %d (want top-5 live set {4,5,6,7,8}, got %v)", live, got)
		}
	}
	// Ranks must be 1..5 in hit order (not the sparse ranks the
	// raw ANN query assigned).
	for i, h := range hits {
		if h.Rank != i+1 {
			t.Errorf("hit[%d].Rank = %d, want %d (post-filter must re-number)", i, h.Rank, i+1)
		}
	}
}

// TestBackend_Search_IterativelyExpandsWhenDeletionsExceedOverfetch
// locks in the fallback path: when soft-deleted messages occupy more
// than deletedOverfetchFactor * k of the top ANN hits, a single 2×
// over-fetch isn't enough. Search must keep doubling fetch until it
// collects k live hits or exhausts the generation.
func TestBackend_Search_IterativelyExpandsWhenDeletionsExceedOverfetch(t *testing.T) {
	b, ctx, _ := newFusedBackendForTest(t)

	if _, err := b.mainDB.ExecContext(ctx,
		`DELETE FROM messages; DELETE FROM messages_fts; DELETE FROM message_recipients`); err != nil {
		t.Fatalf("reset: %v", err)
	}

	// Seed 6 deleted messages at distance 0 plus 5 live messages at
	// graded distances. With k=3, the opening 2× over-fetch of 6
	// returns only deleted rows (0 live). The iterative path must
	// double fetch to 12 and surface live hits {7,8,9}.
	if _, err := b.mainDB.ExecContext(ctx, `
		INSERT INTO messages (id, deleted_from_source_at) VALUES
		    (1, '2026-01-01'), (2, '2026-01-01'), (3, '2026-01-01'),
		    (4, '2026-01-01'), (5, '2026-01-01'), (6, '2026-01-01'),
		    (7, NULL), (8, NULL), (9, NULL), (10, NULL), (11, NULL)`); err != nil {
		t.Fatalf("insert messages: %v", err)
	}

	gid, err := b.CreateGeneration(ctx, "m", 768)
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}

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
	if err := b.Upsert(ctx, gid, chunks); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	hits, err := b.Search(ctx, gid, unitVec(768, 0), 3, vector.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("len(hits) = %d, want 3 (iterative expansion must cover >k deletions)", len(hits))
	}
	wantIDs := map[int64]bool{7: true, 8: true, 9: true}
	for _, h := range hits {
		if !wantIDs[h.MessageID] {
			t.Errorf("unexpected hit id=%d (want any of {7,8,9})", h.MessageID)
		}
	}
	for i, h := range hits {
		if h.Rank != i+1 {
			t.Errorf("hit[%d].Rank = %d, want %d", i, h.Rank, i+1)
		}
	}
}

// TestBackend_Search_ExhaustedCorpusReturnsWhatsAvailable guards the
// termination case: if k exceeds the number of live vectors even
// after expanding to the whole generation, Search returns the
// remainder without looping forever.
func TestBackend_Search_ExhaustedCorpusReturnsWhatsAvailable(t *testing.T) {
	b, ctx, _ := newFusedBackendForTest(t)

	if _, err := b.mainDB.ExecContext(ctx,
		`DELETE FROM messages; DELETE FROM messages_fts; DELETE FROM message_recipients`); err != nil {
		t.Fatalf("reset: %v", err)
	}

	// Seed 3 deleted and 2 live messages. Request k=4: even the full
	// corpus sweep only produces 2 live hits, so Search must return 2
	// rather than loop.
	if _, err := b.mainDB.ExecContext(ctx, `
		INSERT INTO messages (id, deleted_from_source_at) VALUES
		    (1, '2026-01-01'), (2, '2026-01-01'), (3, '2026-01-01'),
		    (4, NULL), (5, NULL)`); err != nil {
		t.Fatalf("insert messages: %v", err)
	}

	gid, err := b.CreateGeneration(ctx, "m", 768)
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}
	chunks := []vector.Chunk{
		{MessageID: 1, Vector: unitVec(768, 0)},
		{MessageID: 2, Vector: unitVec(768, 0)},
		{MessageID: 3, Vector: unitVec(768, 0)},
		{MessageID: 4, Vector: unitVec(768, 1)},
		{MessageID: 5, Vector: unitVec(768, 2)},
	}
	if err := b.Upsert(ctx, gid, chunks); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	hits, err := b.Search(ctx, gid, unitVec(768, 0), 4, vector.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("len(hits) = %d, want 2 (only 2 live messages exist)", len(hits))
	}
}

func TestBackend_Delete_RemovesFromAllTables(t *testing.T) {
	b, ctx := newBackendForTest(t)
	gid := seedAndEmbed(t, b, map[int64][]float32{1: unitVec(768, 0)})

	if err := b.Delete(ctx, gid, []int64{1}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	var n int
	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE message_id = 1`).Scan(&n); err != nil {
		t.Fatalf("count embeddings: %v", err)
	}
	if n != 0 {
		t.Errorf("embeddings remaining: %d", n)
	}
	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM vectors_vec_d768 WHERE message_id = 1`).Scan(&n); err != nil {
		t.Fatalf("count vectors: %v", err)
	}
	if n != 0 {
		t.Errorf("vectors remaining: %d", n)
	}
}

func TestBackend_Delete_EmptyIDsIsNoop(t *testing.T) {
	b, ctx := newBackendForTest(t)
	gid, err := b.CreateGeneration(ctx, "m", 768)
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}
	if err := b.Delete(ctx, gid, nil); err != nil {
		t.Errorf("Delete(nil): %v", err)
	}
	if err := b.Delete(ctx, gid, []int64{}); err != nil {
		t.Errorf("Delete(empty): %v", err)
	}
}

func TestBackend_Delete_UnknownGeneration(t *testing.T) {
	b, ctx := newBackendForTest(t)
	err := b.Delete(ctx, vector.GenerationID(9999), []int64{1})
	if !errors.Is(err, vector.ErrUnknownGeneration) {
		t.Errorf("err = %v, want ErrUnknownGeneration", err)
	}
}

func TestBackend_Stats_CountsCorrectly(t *testing.T) {
	b, ctx := newBackendForTest(t)
	gid := seedAndEmbed(t, b, map[int64][]float32{1: unitVec(768, 0)})

	s, err := b.Stats(ctx, gid)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if s.EmbeddingCount != 1 {
		t.Errorf("EmbeddingCount=%d want 1", s.EmbeddingCount)
	}
	if s.PendingCount != 0 {
		t.Errorf("PendingCount=%d want 0", s.PendingCount)
	}
}

func TestBackend_Stats_PendingCountAfterCreate(t *testing.T) {
	b, ctx := newBackendForTest(t)
	gid, err := b.CreateGeneration(ctx, "m", 768)
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}
	// CreateGeneration seeds 1 pending row for the one pre-seeded message.
	s, err := b.Stats(ctx, gid)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if s.EmbeddingCount != 0 {
		t.Errorf("EmbeddingCount=%d want 0", s.EmbeddingCount)
	}
	if s.PendingCount != 1 {
		t.Errorf("PendingCount=%d want 1", s.PendingCount)
	}
}

func TestBackend_Stats_AggregateAcrossGenerations(t *testing.T) {
	// When gen == 0, Stats returns counts across ALL generations.
	b, ctx := newBackendForTest(t)
	_ = seedAndEmbed(t, b, map[int64][]float32{1: unitVec(768, 0)})

	s, err := b.Stats(ctx, vector.GenerationID(0))
	if err != nil {
		t.Fatalf("Stats(0): %v", err)
	}
	if s.EmbeddingCount != 1 {
		t.Errorf("aggregate EmbeddingCount=%d want 1", s.EmbeddingCount)
	}
}

// TestBackend_Upsert_UpdatesMessageCount verifies that
// index_generations.message_count tracks the number of embedded
// messages after both the initial insert and subsequent re-upsert /
// delete. Without this, ActiveGeneration().MessageCount stays at zero
// regardless of how many chunks have been written.
func TestBackend_Upsert_UpdatesMessageCount(t *testing.T) {
	b, ctx := newBackendForTest(t)

	gid, err := b.CreateGeneration(ctx, "m", 768)
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}

	// Initially zero.
	bg, err := b.BuildingGeneration(ctx)
	if err != nil {
		t.Fatalf("BuildingGeneration: %v", err)
	}
	if bg.MessageCount != 0 {
		t.Errorf("initial MessageCount=%d, want 0", bg.MessageCount)
	}

	// Upsert three chunks → count 3.
	chunks := []vector.Chunk{
		{MessageID: 1, Vector: unitVec(768, 0), SourceCharLen: 10},
		{MessageID: 2, Vector: unitVec(768, 1), SourceCharLen: 20},
		{MessageID: 3, Vector: unitVec(768, 2), SourceCharLen: 30},
	}
	if err := b.Upsert(ctx, gid, chunks); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	bg, err = b.BuildingGeneration(ctx)
	if err != nil {
		t.Fatalf("BuildingGeneration: %v", err)
	}
	if bg.MessageCount != 3 {
		t.Errorf("after initial Upsert: MessageCount=%d, want 3", bg.MessageCount)
	}

	// Re-upsert the same messages (update, not insert) → count stays 3.
	if err := b.Upsert(ctx, gid, chunks[:2]); err != nil {
		t.Fatalf("re-Upsert: %v", err)
	}
	bg, err = b.BuildingGeneration(ctx)
	if err != nil {
		t.Fatalf("BuildingGeneration: %v", err)
	}
	if bg.MessageCount != 3 {
		t.Errorf("after re-Upsert: MessageCount=%d, want 3", bg.MessageCount)
	}

	// Delete one → count drops to 2.
	if err := b.Delete(ctx, gid, []int64{2}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	bg, err = b.BuildingGeneration(ctx)
	if err != nil {
		t.Fatalf("BuildingGeneration: %v", err)
	}
	if bg.MessageCount != 2 {
		t.Errorf("after Delete: MessageCount=%d, want 2", bg.MessageCount)
	}
}

// TestBackend_Stats_UnknownGeneration confirms that passing a non-zero
// generation id that doesn't exist returns an error wrapping
// vector.ErrUnknownGeneration, rather than silently reporting 0 counts
// (which would be indistinguishable from a valid-but-empty generation).
func TestBackend_Stats_UnknownGeneration(t *testing.T) {
	b, ctx := newBackendForTest(t)

	_, err := b.Stats(ctx, vector.GenerationID(9999))
	if err == nil {
		t.Fatal("Stats on unknown generation: want error, got nil")
	}
	if !errors.Is(err, vector.ErrUnknownGeneration) {
		t.Errorf("error = %v, want wrapping ErrUnknownGeneration", err)
	}
}

func TestBackend_LoadVector(t *testing.T) {
	b, ctx := newBackendForTest(t)
	gid, err := b.CreateGeneration(ctx, "m", 768)
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}

	vec := make([]float32, 768)
	for i := range vec {
		vec[i] = float32(i) * 0.01
	}
	chunks := []vector.Chunk{{MessageID: 1, Vector: vec, SourceCharLen: 42}}
	if err := b.Upsert(ctx, gid, chunks); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := b.ActivateGeneration(ctx, gid); err != nil {
		t.Fatalf("ActivateGeneration: %v", err)
	}

	got, err := b.LoadVector(ctx, 1)
	if err != nil {
		t.Fatalf("LoadVector: %v", err)
	}
	if len(got) != 768 {
		t.Fatalf("len=%d, want 768", len(got))
	}
	for i, v := range got {
		if v != vec[i] {
			t.Fatalf("mismatch at i=%d: got %f, want %f", i, v, vec[i])
		}
	}
}

func TestBackend_LoadVector_NotEmbedded(t *testing.T) {
	b, ctx := newBackendForTest(t)
	gid, err := b.CreateGeneration(ctx, "m", 768)
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}

	vec := make([]float32, 768)
	for i := range vec {
		vec[i] = 0.1
	}
	chunks := []vector.Chunk{{MessageID: 1, Vector: vec, SourceCharLen: 42}}
	if err := b.Upsert(ctx, gid, chunks); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := b.ActivateGeneration(ctx, gid); err != nil {
		t.Fatalf("ActivateGeneration: %v", err)
	}

	_, err = b.LoadVector(ctx, 999)
	if err == nil {
		t.Fatal("LoadVector for missing message should error")
	}
}

func TestBackend_LoadVector_NoActive(t *testing.T) {
	b, ctx := newBackendForTest(t)
	_, err := b.LoadVector(ctx, 1)
	if err == nil || !errors.Is(err, vector.ErrNoActiveGeneration) {
		t.Fatalf("want ErrNoActiveGeneration, got %v", err)
	}
}

// TestBackend_Search_ExcludesDedupHidden confirms that Search excludes
// messages hidden by dedup (deleted_at IS NOT NULL), not just those
// deleted from source. Uses a minimal main DB without FTS5.
func TestBackend_Search_ExcludesDedupHidden(t *testing.T) {
	ctx := context.Background()

	// Minimal main DB: two messages, one dedup-hidden. No FTS5 required.
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open main: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE messages (
		id INTEGER PRIMARY KEY,
		deleted_at DATETIME,
		deleted_from_source_at DATETIME
	)`); err != nil {
		t.Fatalf("create messages: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO messages (id, deleted_at) VALUES (1, NULL), (2, '2026-01-01 00:00:00')`); err != nil {
		t.Fatalf("insert messages: %v", err)
	}

	b, err := Open(ctx, Options{
		Path:      t.TempDir() + "/vectors.db",
		Dimension: 768,
		MainDB:    db,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	gid, err := b.CreateGeneration(ctx, "m", 768)
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}
	chunks := []vector.Chunk{
		{MessageID: 1, Vector: unitVec(768, 0)},
		{MessageID: 2, Vector: unitVec(768, 0)},
	}
	if err := b.Upsert(ctx, gid, chunks); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	hits, err := b.Search(ctx, gid, unitVec(768, 0), 10, vector.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := make(map[int64]bool, len(hits))
	for _, h := range hits {
		got[h.MessageID] = true
	}
	if !got[1] {
		t.Errorf("msg 1 missing (live message must appear)")
	}
	if got[2] {
		t.Errorf("msg 2 present (deleted_at IS NOT NULL, must be excluded)")
	}
}

// TestBackend_FilteredMessageIDs_ExcludesDedupHidden confirms that
// filteredMessageIDs excludes messages with deleted_at set.
// Uses a minimal main DB without FTS5.
func TestBackend_FilteredMessageIDs_ExcludesDedupHidden(t *testing.T) {
	ctx := context.Background()

	// Minimal main DB with source_id for SourceIDs filter.
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open main: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE messages (
		id INTEGER PRIMARY KEY,
		source_id INTEGER,
		deleted_at DATETIME,
		deleted_from_source_at DATETIME
	)`); err != nil {
		t.Fatalf("create messages: %v", err)
	}
	// Three messages: 1 live, 2 dedup-hidden, 3 source-deleted.
	if _, err := db.Exec(`
		INSERT INTO messages (id, source_id, deleted_at, deleted_from_source_at) VALUES
		(1, 1, NULL, NULL),
		(2, 1, '2026-01-01 00:00:00', NULL),
		(3, 1, NULL, '2026-01-01 00:00:00')`); err != nil {
		t.Fatalf("insert messages: %v", err)
	}

	b, err := Open(ctx, Options{
		Path:      t.TempDir() + "/vectors.db",
		Dimension: 768,
		MainDB:    db,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	// Upsert vectors for all three messages directly.
	gid, err := b.CreateGeneration(ctx, "m", 768)
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}
	chunks := []vector.Chunk{
		{MessageID: 1, Vector: unitVec(768, 0)},
		{MessageID: 2, Vector: unitVec(768, 0)},
		{MessageID: 3, Vector: unitVec(768, 0)},
	}
	if err := b.Upsert(ctx, gid, chunks); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Filtered search via a non-empty filter triggers filteredMessageIDs.
	hits, err := b.Search(ctx, gid, unitVec(768, 0), 10, vector.Filter{SourceIDs: []int64{1}})
	if err != nil {
		t.Fatalf("Search with filter: %v", err)
	}
	got := make(map[int64]bool, len(hits))
	for _, h := range hits {
		got[h.MessageID] = true
	}
	if got[2] {
		t.Errorf("msg 2 present (deleted_at, must be excluded)")
	}
	if got[3] {
		t.Errorf("msg 3 present (deleted_from_source_at, must be excluded)")
	}
}
