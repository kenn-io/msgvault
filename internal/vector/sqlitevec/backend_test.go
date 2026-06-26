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

	require.NoError(b.ActivateGeneration(ctx, gid, true), "ActivateGeneration")
	g, err := b.ActiveGeneration(ctx)
	require.NoError(err, "ActiveGeneration after activate")
	assert.Equal(vector.GenerationActive, g.State)
	assert.Equal("nomic-embed-text-v1.5:768", g.Fingerprint)

	require.NoError(b.RetireGeneration(ctx, gid, true), "RetireGeneration")
	_, err = b.ActiveGeneration(ctx)
	require.Error(err, "ActiveGeneration should error after retire")
}

// missingCountSV returns the number of live messages still needing
// embedding for gen (embed_gen <> gen) in the backend's main DB. This is
// the scan-and-fill coverage count that replaced pending_embeddings.
func missingCountSV(t *testing.T, b *Backend, gen vector.GenerationID) int {
	t.Helper()
	missing, err := b.hasMissingForGen(context.Background(), gen)
	requirepkg.NoError(t, err, "hasMissingForGen")
	if missing {
		return 1
	}
	return 0
}

// genStateSV reads index_generations.state for a generation.
func genStateSV(t *testing.T, b *Backend, gen vector.GenerationID) vector.GenerationState {
	t.Helper()
	var s vector.GenerationState
	requirepkg.NoError(t, b.db.QueryRowContext(context.Background(),
		`SELECT state FROM index_generations WHERE id = ?`, int64(gen)).Scan(&s),
		"read state for generation %d", gen)
	return s
}

// TestBackend_RetireGeneration_ActiveGuard pins the retire-TOCTOU class-closing
// fix: the active-gen guard lives ATOMICALLY inside RetireGeneration's tx.
//   - force=false against the ACTIVE generation is refused with
//     ErrRefuseRetireActive, leaving state='active'.
//   - force=true retires the active generation.
//   - force=false against a NON-active (building) generation retires fine.
func TestBackend_RetireGeneration_ActiveGuard(t *testing.T) {
	b, ctx := newBackendForTest(t)

	// Build + force-activate genA (force bypasses the coverage gate so the
	// one unembedded test message does not block activation).
	genA, err := b.CreateGeneration(ctx, "model-a", 768, "")
	requirepkg.NoError(t, err, "CreateGeneration A")
	requirepkg.NoError(t, b.ActivateGeneration(ctx, genA, true), "activate A (force)")
	requirepkg.Equal(t, vector.GenerationActive, genStateSV(t, b, genA), "precondition: A active")

	// (1) Non-forced retire of the ACTIVE gen is refused atomically.
	err = b.RetireGeneration(ctx, genA, false)
	requirepkg.ErrorIs(t, err, vector.ErrRefuseRetireActive,
		"non-forced retire of active gen must return ErrRefuseRetireActive")
	assertpkg.Equal(t, vector.GenerationActive, genStateSV(t, b, genA),
		"refused retire must leave the active gen's state unchanged")

	// (2) Forced retire succeeds.
	requirepkg.NoError(t, b.RetireGeneration(ctx, genA, true),
		"forced retire of active gen must succeed")
	assertpkg.Equal(t, vector.GenerationRetired, genStateSV(t, b, genA),
		"forced retire flips state to retired")

	// (3) A NON-active (building) generation retires fine without force.
	genB, err := b.CreateGeneration(ctx, "model-b", 768, "")
	requirepkg.NoError(t, err, "CreateGeneration B")
	requirepkg.Equal(t, vector.GenerationBuilding, genStateSV(t, b, genB), "precondition: B building")
	requirepkg.NoError(t, b.RetireGeneration(ctx, genB, false),
		"non-forced retire of a non-active gen must succeed")
	assertpkg.Equal(t, vector.GenerationRetired, genStateSV(t, b, genB),
		"non-active gen retires to retired without force")
}

// TestBackend_ActivateGeneration_AutoRetires pins the auto-retire path:
// activating a new generation demotes the previously-active one to
// retired in the same tx as the state flip (RETURNING-id provable).
func TestBackend_ActivateGeneration_AutoRetires(t *testing.T) {
	b, ctx := newBackendForTest(t)

	genA, err := b.CreateGeneration(ctx, "model-a", 768, "")
	requirepkg.NoError(t, err, "CreateGeneration A")
	requirepkg.NoError(t, b.ActivateGeneration(ctx, genA, true), "activate A (force)")

	genB, err := b.CreateGeneration(ctx, "model-b", 768, "")
	requirepkg.NoError(t, err, "CreateGeneration B")
	requirepkg.NoError(t, b.ActivateGeneration(ctx, genB, true), "activate B (auto-retires A)")

	retired := singleRetiredGenSV(t, b)
	assertpkg.Equal(t, genA, retired, "the previously-active gen must be the sole retired row")
	assertpkg.NotEqual(t, genB, retired, "the newly-activated gen must not be retired")
}

// TestBackend_ActivateGeneration_CoverageGate pins the scan-and-fill
// activation gate: a generation with a live message still needing
// embedding (embed_gen <> gen) is refused without force, and succeeds once
// coverage is complete (or with force).
func TestBackend_ActivateGeneration_CoverageGate(t *testing.T) {
	b, ctx := newBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "m", 768, "")
	requirepkg.NoError(t, err, "CreateGeneration")
	requirepkg.Equal(t, 1, missingCountSV(t, b, gen), "precondition: one missing message")

	// Non-forced activate is refused while the message is unembedded.
	err = b.ActivateGeneration(ctx, gen, false)
	requirepkg.Error(t, err, "activate must be refused with missing coverage")
	assertpkg.Contains(t, err.Error(), "needing embedding")

	// Stamp the message as covered, then activation succeeds.
	_, err = b.mainDB.ExecContext(ctx, `UPDATE messages SET embed_gen = ? WHERE id = 1`, int64(gen))
	requirepkg.NoError(t, err, "stamp embed_gen")
	requirepkg.Equal(t, 0, missingCountSV(t, b, gen), "covered now")
	requirepkg.NoError(t, b.ActivateGeneration(ctx, gen, false), "activate after coverage complete")
	assertpkg.Equal(t, vector.GenerationActive, genStateSV(t, b, gen), "now active")
}

// TestBackend_ActivateGeneration_LifecycleErrorBeforeCoverage pins that
// activating an unknown or non-building generation WITHOUT --force returns
// the lifecycle error (unknown generation / not in 'building' state), NOT
// the misleading "messages needing embedding" coverage error. The coverage
// predicate (embed_gen IS NULL OR embed_gen <> gen) is true for an unknown
// gen id, so the lifecycle check must run first. The seeded test message
// (id=1) stays unembedded so the coverage gate WOULD trip if checked first.
func TestBackend_ActivateGeneration_LifecycleErrorBeforeCoverage(t *testing.T) {
	b, ctx := newBackendForTest(t)
	requirepkg.Equal(t, 1, missingCountSV(t, b, vector.GenerationID(999)),
		"precondition: coverage gate would trip for any gen (message unembedded)")

	// (a) Unknown gen id: lifecycle error (ErrUnknownGeneration), not coverage.
	err := b.ActivateGeneration(ctx, vector.GenerationID(999), false)
	requirepkg.Error(t, err, "activating unknown gen must fail")
	requirepkg.ErrorIs(t, err, vector.ErrUnknownGeneration,
		"unknown gen must return ErrUnknownGeneration, not coverage error")
	assertpkg.NotContains(t, err.Error(), "needing embedding",
		"unknown gen must NOT surface the coverage error")

	// (b) Non-building (retired) gen id: lifecycle error, not coverage.
	gen, err := b.CreateGeneration(ctx, "m", 768, "")
	requirepkg.NoError(t, err, "CreateGeneration")
	requirepkg.NoError(t, b.ActivateGeneration(ctx, gen, true), "force-activate to bypass coverage")
	requirepkg.NoError(t, b.RetireGeneration(ctx, gen, true), "force-retire to reach non-building state")
	requirepkg.Equal(t, vector.GenerationRetired, genStateSV(t, b, gen), "precondition: gen retired")

	err = b.ActivateGeneration(ctx, gen, false)
	requirepkg.Error(t, err, "activating retired gen must fail")
	assertpkg.Contains(t, err.Error(), "not in 'building' state",
		"retired gen must return the not-building lifecycle error")
	assertpkg.NotContains(t, err.Error(), "needing embedding",
		"retired gen must NOT surface the coverage error")
}

// TestBackend_SingleTargetRebuild pins the single-target invariant: while
// a new generation B builds, the active generation A keeps serving
// (stale-but-correct), and B only becomes active once its coverage is
// complete — at which point A is retired in the same swap. There is no
// dual-write fan-out; the per-message embed_gen names exactly one target.
func TestBackend_SingleTargetRebuild(t *testing.T) {
	b, ctx := newBackendForTest(t)

	// A: build, cover (force-activate to skip the gate for the one test
	// message), and start serving.
	genA, err := b.CreateGeneration(ctx, "model-a", 768, "")
	requirepkg.NoError(t, err, "CreateGeneration A")
	requirepkg.NoError(t, b.ActivateGeneration(ctx, genA, true), "activate A (force)")
	active, err := b.ActiveGeneration(ctx)
	requirepkg.NoError(t, err, "ActiveGeneration")
	requirepkg.Equal(t, genA, active.ID, "A is serving")

	// B: a new building generation for the same corpus. The message reads
	// as missing for B (embed_gen still names A), but A keeps serving
	// unchanged — stale-but-correct mid-rebuild.
	genB, err := b.CreateGeneration(ctx, "model-b", 768, "")
	requirepkg.NoError(t, err, "CreateGeneration B")
	requirepkg.Equal(t, 1, missingCountSV(t, b, genB), "message missing for B mid-rebuild")
	active, err = b.ActiveGeneration(ctx)
	requirepkg.NoError(t, err, "ActiveGeneration mid-rebuild")
	assertpkg.Equal(t, genA, active.ID, "A still serving while B builds")

	// B's activation is refused until its coverage is complete.
	requirepkg.Error(t, b.ActivateGeneration(ctx, genB, false), "B refused while incomplete")

	// Cover the message for B (worker would do this after upsert), then
	// activate B — the swap retires A and makes B the single serving gen.
	_, err = b.mainDB.ExecContext(ctx, `UPDATE messages SET embed_gen = ? WHERE id = 1`, int64(genB))
	requirepkg.NoError(t, err, "stamp embed_gen for B")
	requirepkg.NoError(t, b.ActivateGeneration(ctx, genB, false), "activate B after coverage complete")

	active, err = b.ActiveGeneration(ctx)
	requirepkg.NoError(t, err, "ActiveGeneration after swap")
	assertpkg.Equal(t, genB, active.ID, "B is the single serving gen after swap")
	assertpkg.Equal(t, vector.GenerationRetired, genStateSV(t, b, genA), "A retired by the swap")
}

// singleRetiredGenSV returns the id of the one generation in state='retired',
// failing if there is not exactly one. Used by the auto-retire RETURNING-id
// test to prove the reaped id is the same row whose state flipped to retired.
func singleRetiredGenSV(t *testing.T, b *Backend) vector.GenerationID {
	t.Helper()
	var id int64
	requirepkg.NoError(t, b.db.QueryRowContext(context.Background(),
		`SELECT id FROM index_generations WHERE state = 'retired'`).Scan(&id),
		"expected exactly one retired generation")
	return vector.GenerationID(id)
}

// TestBackend_CreateGeneration_StampsSeededAt confirms CreateGeneration
// stamps seeded_at so the activation gate's lifecycle check passes.
func TestBackend_CreateGeneration_StampsSeededAt(t *testing.T) {
	b, ctx := newBackendForTest(t)
	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	requirepkg.NoError(t, err, "Create")
	var seededAt sql.NullInt64
	requirepkg.NoError(t, b.db.QueryRowContext(ctx,
		`SELECT seeded_at FROM index_generations WHERE id = ?`, int64(gid)).Scan(&seededAt))
	assertpkg.True(t, seededAt.Valid, "seeded_at stamped at creation")
}

// TestBackend_ActivateGeneration_NullSeededAtActivatesWithCoverage pins
// that a legacy/crashed generation whose seeded_at is NULL must still
// activate WITHOUT --force as long as coverage is complete (missing==0).
// The old seeded_at IS NOT NULL gate would have rejected it and pointed
// users at `embeddings resume`, which cannot stamp seeded_at — making the
// row unactivatable except via --force. Coverage is the real gate now.
func TestBackend_ActivateGeneration_NullSeededAtActivatesWithCoverage(t *testing.T) {
	b, ctx := newBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "m", 768, "")
	requirepkg.NoError(t, err, "CreateGeneration")

	// Simulate a legacy/crashed generation: clear seeded_at.
	_, err = b.db.ExecContext(ctx,
		`UPDATE index_generations SET seeded_at = NULL WHERE id = ?`, int64(gen))
	requirepkg.NoError(t, err, "clear seeded_at")
	var seededAt sql.NullInt64
	requirepkg.NoError(t, b.db.QueryRowContext(ctx,
		`SELECT seeded_at FROM index_generations WHERE id = ?`, int64(gen)).Scan(&seededAt))
	requirepkg.False(t, seededAt.Valid, "precondition: seeded_at is NULL")

	// Make coverage complete (worker would stamp this after upsert).
	_, err = b.mainDB.ExecContext(ctx, `UPDATE messages SET embed_gen = ? WHERE id = 1`, int64(gen))
	requirepkg.NoError(t, err, "stamp embed_gen")
	requirepkg.Equal(t, 0, missingCountSV(t, b, gen), "precondition: coverage complete")

	// Activation succeeds WITHOUT force despite seeded_at=NULL.
	requirepkg.NoError(t, b.ActivateGeneration(ctx, gen, false),
		"NULL seeded_at + full coverage must activate without --force")
	assertpkg.Equal(t, vector.GenerationActive, genStateSV(t, b, gen), "now active")
}

func TestBackend_CreateGeneration_ScopeLimitsCoverage(t *testing.T) {
	ctx := context.Background()
	main, err := sql.Open("sqlite3", ":memory:")
	requirepkg.NoError(t, err, "open main")
	t.Cleanup(func() { _ = main.Close() })
	_, err = main.Exec(`CREATE TABLE messages (
		id INTEGER PRIMARY KEY,
		message_type TEXT NOT NULL,
		embed_gen INTEGER,
		deleted_at DATETIME,
		deleted_from_source_at DATETIME
	)`)
	requirepkg.NoError(t, err, "create messages")
	_, err = main.Exec(`
		INSERT INTO messages (id, message_type, deleted_from_source_at) VALUES
		(1, 'email', NULL),
		(2, 'sms', NULL),
		(3, 'mms', NULL),
		(4, 'sms', CURRENT_TIMESTAMP)`)
	requirepkg.NoError(t, err, "insert messages")

	b, err := Open(ctx, Options{
		Path:       filepath.Join(t.TempDir(), "vectors.db"),
		Dimension:  768,
		MainDB:     main,
		BuildScope: vector.NewBuildScope([]string{"sms", "mms"}),
	})
	requirepkg.NoError(t, err, "Open")
	t.Cleanup(func() { _ = b.Close() })

	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	requirepkg.NoError(t, err, "Create")
	stats, err := b.Stats(ctx, gid)
	requirepkg.NoError(t, err, "Stats")
	assertpkg.Equal(t, int64(2), stats.PendingCount, "only scoped live messages should count as missing")

	missing, err := b.hasMissingForGen(ctx, gid)
	requirepkg.NoError(t, err, "hasMissingForGen")
	assertpkg.True(t, missing, "scoped messages still need embeddings")

	_, err = main.Exec(`UPDATE messages SET embed_gen = ? WHERE id IN (2, 3)`, int64(gid))
	requirepkg.NoError(t, err, "stamp scoped messages")

	stats, err = b.Stats(ctx, gid)
	requirepkg.NoError(t, err, "Stats after stamp")
	assertpkg.Equal(t, int64(0), stats.PendingCount, "unscoped email must not block scoped coverage")
	missing, err = b.hasMissingForGen(ctx, gid)
	requirepkg.NoError(t, err, "hasMissingForGen after stamp")
	assertpkg.False(t, missing, "unscoped email must not block scoped activation")
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

// TestBackend_CoverageGate_SkipsDeletedMessages verifies the coverage
// gate's live-message predicate excludes soft-deleted rows: a backend
// whose only message is deleted-from-source reports zero missing, so a
// building generation can activate without force.
func TestBackend_CoverageGate_SkipsDeletedMessages(t *testing.T) {
	b := openBackendWithOneDeletedMessage(t)
	t.Cleanup(func() { _ = b.Close() })
	ctx := context.Background()
	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	requirepkg.NoError(t, err, "Create")
	missing, err := b.hasMissingForGen(ctx, gid)
	requirepkg.NoError(t, err, "hasMissingForGen")
	assertpkg.False(t, missing, "deleted-from-source message must not count as missing")
	// With no live missing message, activation passes the coverage gate.
	requirepkg.NoError(t, b.ActivateGeneration(ctx, gid, false), "activate (no missing)")
}

// TestBackend_CoverageGate_SkipsDedupHidden verifies the coverage gate
// excludes dedup-hidden messages (deleted_at IS NOT NULL): only the live
// message counts toward "missing".
func TestBackend_CoverageGate_SkipsDedupHidden(t *testing.T) {
	require := requirepkg.New(t)
	ctx := context.Background()

	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(err, "open main")
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`CREATE TABLE messages (
		id INTEGER PRIMARY KEY,
		deleted_at DATETIME,
		deleted_from_source_at DATETIME,
		embed_gen INTEGER
	)`)
	require.NoError(err, "create messages")
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
	// Stamp the dedup-hidden message as if covered for some other gen; it
	// must still not count (it is dedup-hidden), and the live one must.
	s, err := b.Stats(ctx, gid)
	require.NoError(err, "Stats")
	assertpkg.Equal(t, int64(1), s.PendingCount, "only the live message counts as missing")
}

// TestBackend_Upsert_WritesEmbeddingAndVector verifies Upsert's
// contract: it writes the embeddings row and the dimension-specific
// vec0 row, and explicitly does NOT touch messages.embed_gen (the worker
// stamps that after a successful upsert, in the main DB).
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

	// embed_gen is untouched by Upsert — message 1 still reads as missing.
	var embedGen sql.NullInt64
	err = b.mainDB.QueryRowContext(ctx,
		`SELECT embed_gen FROM messages WHERE id = 1`).Scan(&embedGen)
	require.NoError(err, "read embed_gen")
	assert.False(embedGen.Valid, "Upsert must not stamp messages.embed_gen")
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
	b, ctx := newFusedBackendForTest(t)

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
	b, ctx := newFusedBackendForTest(t)

	// Reset and seed 4 messages with distinct recipient / size / subject
	// profiles so each assertion is unambiguous.
	_, err := b.mainDB.ExecContext(ctx,
		`DELETE FROM messages; DELETE FROM messages_fts; DELETE FROM message_recipients`)
	require.NoError(err, "reset")

	rows := []struct {
		id          int64
		size        int64
		subject     string
		messageType string
		to, cc      int64
	}{
		{1, 100_000, "quarterly planning", "email", 10, 0},
		{2, 5_000_000, "quarterly review", "sms", 20, 10},
		{3, 100_000, "lunch", "sms", 20, 0},
		{4, 20_000_000, "quarterly deep dive", "email", 30, 0},
	}
	for _, r := range rows {
		_, err := b.mainDB.ExecContext(ctx,
			`INSERT INTO messages (id, subject, size_estimate, message_type) VALUES (?, ?, ?, ?)`,
			r.id, r.subject, r.size, r.messageType)
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
	t.Run("MessageTypes", func(t *testing.T) {
		got := matched(t, vector.Filter{MessageTypes: []string{"sms"}})
		assertpkg.Truef(t, got[2] && got[3] && !got[1] && !got[4], "MessageTypes=[sms]: got %v, want {2,3}", got)
	})
	t.Run("CombinedFilter", func(t *testing.T) {
		size := int64(1_000_000)
		got := matched(t, vector.Filter{
			ToGroups:          [][]int64{{20}},
			LargerThan:        &size,
			SubjectSubstrings: []string{"quarterly"},
			MessageTypes:      []string{"sms"},
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
	b, ctx := newFusedBackendForTest(t)

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
	b, ctx := newFusedBackendForTest(t)

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
	b, ctx := newFusedBackendForTest(t)

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
	b, ctx := newFusedBackendForTest(t)

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
	b, ctx := newFusedBackendForTest(t)

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
	b, ctx := newFusedBackendForTest(t)

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
	b, ctx := newFusedBackendForTest(t)

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
	// The one live message is unembedded, so PendingCount (= missing for
	// this gen, read from the main DB) is 1.
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
	require.NoError(b.ActivateGeneration(ctx, genA, true), "ActivateGeneration(genA)")

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
	require.NoError(b.ActivateGeneration(ctx, gid, true), "ActivateGeneration")

	got, err := b.LoadVector(ctx, 1)
	require.NoError(err, "LoadVector")
	require.Len(got, 768)
	for i, v := range got {
		// InDelta (not InEpsilon) because vec[0] == 0 and epsilon is a
		// relative tolerance; the float32 round-trip is exact, so 1e-6
		// is generous.
		require.InDeltaf(vec[i], v, 1e-6, "mismatch at i=%d", i)
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
	require.NoError(b.ActivateGeneration(ctx, gid, true), "ActivateGeneration")

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
