//go:build pgvector

package pgvector

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

// TestBackend_CreateActivateRetire exercises the full lifecycle of one
// generation against pgvector. Parallel to the sqlitevec test of the
// same name (internal/vector/sqlitevec/backend_test.go).
func TestBackend_CreateActivateRetire(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	b, ctx, _ := newBackendForTest(t)

	gid, err := b.CreateGeneration(ctx, "nomic-embed-text-v1.5", 768, "")
	require.NoError(
		err, "CreateGeneration")

	bg, err := b.BuildingGeneration(ctx)
	require.NoError(
		err, "BuildingGeneration")

	require.NotNil(bg, "BuildingGeneration returned nil")
	assert.Equal(gid, bg.ID, "BuildingGeneration id mismatch")

	_, err = b.ActiveGeneration(ctx)
	require.Error(err, "ActiveGeneration should error before activation")
	require.NoError(
		b.ActivateGeneration(ctx, gid, true), "ActivateGeneration")

	g, err := b.ActiveGeneration(ctx)
	require.NoError(
		err, "ActiveGeneration after activate")

	assert.Equal(vector.GenerationActive, g.State, "State want active")
	assert.Equal("nomic-embed-text-v1.5:768", g.Fingerprint, "Fingerprint mismatch")
	require.NoError(
		b.RetireGeneration(ctx, gid, true), "RetireGeneration")

	_, err = b.ActiveGeneration(ctx)
	assert.Error(err, "ActiveGeneration should error after retire")
}

// TestBackend_CreateGeneration_StampsSeededAt verifies CreateGeneration
// stamps seeded_at so the activation gate's lifecycle check passes.
func TestBackend_CreateGeneration_StampsSeededAt(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)
	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(t, err, "Create")

	var seededAt sql.NullInt64
	require.NoError(t, b.db.QueryRowContext(ctx,
		`SELECT seeded_at FROM index_generations WHERE id = $1`, int64(gid)).Scan(&seededAt))

	assert.True(t, seededAt.Valid, "seeded_at stamped at creation")
}

// TestBackend_CoverageGate_SkipsDeleted ensures the coverage gate honours
// the live-message predicate: a soft-deleted message does not count as
// missing, so a building generation can activate without force.
func TestBackend_CoverageGate_SkipsDeleted(t *testing.T) {
	require := require.New(t)

	db := openPGTestDB(t)
	// testSetupPGSchema seeds a live message id=1; mark it deleted so the
	// only message is excluded from the coverage universe.
	_, err := db.Exec(`UPDATE messages SET deleted_from_source_at = NOW() WHERE id = 1`)
	require.NoError(
		err, "soft-delete message")

	ctx := context.Background()
	b, err := Open(ctx, Options{DB: db, Dimension: 768})
	require.NoError(
		err, "Open")

	t.Cleanup(func() { _ = b.Close() })

	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(
		err, "Create")

	s, err := b.Stats(ctx, gid)
	require.NoError(
		err, "Stats")

	assert.Equal(t, int64(0), s.PendingCount, "deleted message must not count as missing")
	require.NoError(
		b.ActivateGeneration(ctx, gid, false), "activate (no missing)")
}

// TestBackend_CreateGeneration_ResumesBuilding checks the idempotent
// resume path: calling CreateGeneration twice with the same fingerprint
// returns the same generation ID instead of failing on the partial
// unique index.
func TestBackend_CreateGeneration_ResumesBuilding(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)

	first, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(t, err, "first Create")
	second, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(t, err, "second Create")
	assert.Equal(t, first, second, "second Create must reuse id")
}

// TestBackend_CreateGeneration_MismatchedFingerprint asserts that a
// second CreateGeneration call with a different fingerprint while
// another build is in progress surfaces ErrBuildingInProgress.
func TestBackend_CreateGeneration_MismatchedFingerprint(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)

	_, err := b.CreateGeneration(ctx, "model-a", 768, "")
	require.NoError(t, err, "first Create")

	_, err = b.CreateGeneration(ctx, "model-b", 768, "")
	require.Error(t, err, "second Create with different fingerprint: want error")
	assert.ErrorIs(t, err, vector.ErrBuildingInProgress,
		"error = %v, want wrapping ErrBuildingInProgress", err)
}

// TestBackend_ActivateGeneration_CoverageGate pins the scan-and-fill
// activation gate (in-tx on PG): a generation with a live message still
// needing embedding (embed_gen <> gen) is refused without force, and
// succeeds once the message is stamped covered.
func TestBackend_ActivateGeneration_CoverageGate(t *testing.T) {
	require := require.New(t)

	b, ctx, db := newBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(
		err, "Create")

	// The seeded live message (id=1) is unembedded -> activation refused.
	err = b.ActivateGeneration(ctx, gen, false)
	require.Error(err, "activate must be refused with missing coverage")
	assert.Contains(t, err.Error(), "needing embedding")

	// Stamp it covered, then activation succeeds.
	_, err = db.ExecContext(ctx, `UPDATE messages SET embed_gen = $1 WHERE id = 1`, int64(gen))
	require.NoError(
		err, "stamp embed_gen")

	require.NoError(
		b.ActivateGeneration(ctx, gen, false), "activate after coverage complete")
}

// TestBackend_ActivateGeneration_LifecycleErrorBeforeCoverage pins that
// activating an unknown or non-building generation WITHOUT --force returns
// the lifecycle error (unknown generation / not in 'building' state), NOT
// the misleading "messages needing embedding" coverage error. The coverage
// predicate (embed_gen <> gen) is true for an unknown gen id, so
// activateGateError must check existence + 'building' state before coverage.
// The seeded test message (id=1) stays unembedded so the coverage gate WOULD
// trip if checked first.
func TestBackend_ActivateGeneration_LifecycleErrorBeforeCoverage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	b, ctx, _ := newBackendForTest(t)

	// (a) Unknown gen id: lifecycle error (ErrUnknownGeneration), not coverage.
	err := b.ActivateGeneration(ctx, vector.GenerationID(999), false)
	require.Error(err, "activating unknown gen must fail")
	require.ErrorIs(err, vector.ErrUnknownGeneration,
		"unknown gen must return ErrUnknownGeneration, not coverage error")
	assert.NotContains(err.Error(), "needing embedding",
		"unknown gen must NOT surface the coverage error")

	// (b) Non-building (retired) gen id: lifecycle error, not coverage.
	gen, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(
		err, "Create")

	require.NoError(
		b.ActivateGeneration(ctx, gen, true), "force-activate to bypass coverage")

	require.NoError(
		b.RetireGeneration(ctx, gen, true), "force-retire to reach non-building state")

	require.Equal(string(vector.GenerationRetired), genState(t, b, gen), "precondition: gen retired")

	err = b.ActivateGeneration(ctx, gen, false)
	require.Error(err, "activating retired gen must fail")
	assert.Contains(err.Error(), "not in 'building' state",
		"retired gen must return the not-building lifecycle error")
	assert.NotContains(err.Error(), "needing embedding",
		"retired gen must NOT surface the coverage error")
}

// TestBackend_ActivateGeneration_NullSeededAtActivatesWithCoverage mirrors
// the sqlitevec lifecycle test: a legacy/crashed generation with seeded_at
// NULL must still activate WITHOUT --force as long as coverage is complete
// (missing==0). The old seeded_at IS NOT NULL gate is gone; coverage is the
// real gate.
func TestBackend_ActivateGeneration_NullSeededAtActivatesWithCoverage(t *testing.T) {
	require := require.New(t)

	b, ctx, db := newBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(
		err, "Create")

	// Simulate a legacy/crashed generation: clear seeded_at.
	_, err = db.ExecContext(ctx,
		`UPDATE index_generations SET seeded_at = NULL WHERE id = $1`, int64(gen))
	require.NoError(
		err, "clear seeded_at")

	var seededAt sql.NullInt64
	require.NoError(b.db.QueryRowContext(ctx,
		`SELECT seeded_at FROM index_generations WHERE id = $1`, int64(gen)).Scan(&seededAt))
	require.False(seededAt.Valid, "precondition: seeded_at is NULL")

	// Make coverage complete (worker would stamp this after upsert).
	_, err = db.ExecContext(ctx, `UPDATE messages SET embed_gen = $1 WHERE id = 1`, int64(gen))
	require.NoError(
		err, "stamp embed_gen")

	require.NoError(
		b.ActivateGeneration(ctx, gen, false),
		"NULL seeded_at + full coverage must activate without --force")
}

// TestBackend_Upsert_RejectsDimensionMismatch ensures the per-chunk
// dimension check fires before any row is written.
func TestBackend_Upsert_RejectsDimensionMismatch(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "m", 4, "")
	require.NoError(t, err, "Create")

	err = b.Upsert(ctx, gen, []vector.Chunk{
		{MessageID: 1, Vector: []float32{1, 2, 3}}, // 3 dims, gen has 4
	})
	assert.ErrorIs(t, err, vector.ErrDimensionMismatch,
		"err=%v, want wrapping ErrDimensionMismatch", err)
}

// TestBackend_Upsert_UnknownGeneration surfaces ErrUnknownGeneration
// when a caller passes a bogus generation id.
func TestBackend_Upsert_UnknownGeneration(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)
	err := b.Upsert(ctx, vector.GenerationID(999), []vector.Chunk{
		{MessageID: 1, Vector: []float32{0, 0, 0, 0}},
	})
	assert.ErrorIs(t, err, vector.ErrUnknownGeneration,
		"err=%v, want wrapping ErrUnknownGeneration", err)
}

// TestBackend_Upsert_Idempotent_PerMessage upserts the same message
// twice and confirms message_count is incremented only once.
func TestBackend_Upsert_Idempotent_PerMessage(t *testing.T) {
	b, ctx, db := newBackendForTest(t)
	gen := seedAndEmbed(t, b, db, map[int64][]float32{
		1: {1, 0, 0, 0},
	})
	err := b.Upsert(ctx, gen, []vector.Chunk{
		{MessageID: 1, Vector: []float32{0, 1, 0, 0}}, // same id, new vector
	})
	require.NoError(t, err, "second Upsert")

	var count int64
	err = b.db.QueryRowContext(ctx,
		`SELECT message_count FROM index_generations WHERE id = $1`, int64(gen)).Scan(&count)
	require.NoError(t, err, "query message_count")
	assert.Equal(t, int64(1), count, "message_count (re-upsert must not double-count)")
}

// TestBackend_Search_FastPath_RanksByDistance exercises the empty-
// filter fast path. The query vector points along axis 0; the message
// whose vector also points along axis 0 must rank first.
func TestBackend_Search_FastPath_RanksByDistance(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	b, ctx, db := newBackendForTest(t)
	gen := seedAndEmbed(t, b, db, map[int64][]float32{
		1: unitVec(4, 0),
		2: unitVec(4, 1),
		3: unitVec(4, 2),
	})
	hits, err := b.Search(ctx, gen, unitVec(4, 0), 3, vector.Filter{})
	require.NoError(
		err, "Search")

	require.NotEmpty(hits, "Search returned no hits")
	assert.Equal(int64(1), hits[0].MessageID, "top hit want 1")
	for i, h := range hits {
		assert.Equal(i+1, h.Rank, "hit[%d].Rank", i)
	}
}

// TestBackend_Search_DropsDeletedFromSource confirms the live-message
// EXISTS clause filters out soft-deleted rows even when their
// embedding would otherwise rank highly.
func TestBackend_Search_DropsDeletedFromSource(t *testing.T) {
	b, ctx, db := newBackendForTest(t)
	gen := seedAndEmbed(t, b, db, map[int64][]float32{
		1: unitVec(4, 0),
		2: unitVec(4, 1),
	})
	// Soft-delete the top hit. Search must skip it and return msg 2.
	_, err := db.ExecContext(ctx,
		`UPDATE messages SET deleted_from_source_at = NOW() WHERE id = 1`)
	require.NoError(t, err, "soft-delete")

	hits, err := b.Search(ctx, gen, unitVec(4, 0), 5, vector.Filter{})
	require.NoError(t, err, "Search")
	for _, h := range hits {
		assert.NotEqual(t, int64(1), h.MessageID,
			"Search returned soft-deleted msg 1 with score %v", h.Score)
	}
}

// TestBackend_Search_RespectsFilter exercises the filtered path. We
// only allow message 2 through the SourceIDs filter; even though
// message 1 has a closer vector, it must be excluded.
func TestBackend_Search_RespectsFilter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	b, ctx, db := newBackendForTest(t)
	gen := seedAndEmbed(t, b, db, map[int64][]float32{
		1: unitVec(4, 0),
		2: unitVec(4, 1),
	})
	// Tag the messages with distinct source_ids so the filter can pick
	// exactly one of them. SourceIDs operates over m.source_id.
	_, err := db.ExecContext(ctx,
		`UPDATE messages SET source_id = 10 WHERE id = 1`)
	require.NoError(
		err, "tag source 10")

	_, err = db.ExecContext(ctx,
		`UPDATE messages SET source_id = 20 WHERE id = 2`)
	require.NoError(
		err, "tag source 20")

	hits, err := b.Search(ctx, gen, unitVec(4, 0), 5, vector.Filter{SourceIDs: []int64{20}})
	require.NoError(
		err, "Search")

	if assert.Len(hits, 1, "filtered hits want exactly 1") {
		assert.Equal(int64(2), hits[0].MessageID, "filtered hit want msg 2")
	}
}

// TestBackend_LoadVector_RoundTrip writes a known vector and reads it
// back, confirming the text format round-trips through pgvector
// without loss for float32 precision.
func TestBackend_LoadVector_RoundTrip(t *testing.T) {
	require := require.New(t)

	b, ctx, db := newBackendForTest(t)
	original := []float32{0.25, -0.5, 0.75, 1.0}
	gen := seedAndEmbed(t, b, db, map[int64][]float32{1: original})
	require.NoError(
		b.ActivateGeneration(ctx, gen, true), "Activate")

	got, err := b.LoadVector(ctx, 1)
	require.NoError(
		err, "LoadVector")

	require.Len(got, len(original), "loaded len")
	for i := range original {
		assert.InDelta(t, original[i], got[i], 0, "dim[%d]", i)
	}
}

// TestBackend_Delete_RemovesAndUpdatesCount confirms Delete removes
// the embedding row and decrements message_count atomically.
func TestBackend_Delete_RemovesAndUpdatesCount(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	b, ctx, db := newBackendForTest(t)
	gen := seedAndEmbed(t, b, db, map[int64][]float32{
		1: unitVec(4, 0),
		2: unitVec(4, 1),
	})
	require.NoError(
		b.Delete(ctx, gen, []int64{1}), "Delete")

	var remaining, msgCount int64
	err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = $1`, int64(gen)).Scan(&remaining)
	require.NoError(
		err, "count")

	err = b.db.QueryRowContext(ctx,
		`SELECT message_count FROM index_generations WHERE id = $1`, int64(gen)).Scan(&msgCount)
	require.NoError(
		err, "message_count")

	assert.Equal(int64(1), remaining, "remaining embeddings")
	assert.Equal(int64(1), msgCount, "message_count")
}

// TestBackend_Stats_ScopedAndAggregate covers both the per-generation
// and aggregate (gen == 0) modes of Stats.
func TestBackend_Stats_ScopedAndAggregate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	b, ctx, db := newBackendForTest(t)
	gen := seedAndEmbed(t, b, db, map[int64][]float32{
		1: unitVec(4, 0),
		2: unitVec(4, 1),
	})

	s, err := b.Stats(ctx, gen)
	require.NoError(
		err, "Stats(gen)")

	assert.Equal(int64(2), s.EmbeddingCount, "scoped EmbeddingCount")

	all, err := b.Stats(ctx, 0)
	require.NoError(
		err, "Stats(0)")

	assert.Equal(int64(2), all.EmbeddingCount, "aggregate EmbeddingCount")
}

// TestBackend_Stats_UnknownGeneration ensures Stats surfaces
// ErrUnknownGeneration when a non-zero generation id has no row.
func TestBackend_Stats_UnknownGeneration(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)
	_, err := b.Stats(ctx, vector.GenerationID(999))
	assert.ErrorIs(t, err, vector.ErrUnknownGeneration,
		"err=%v, want wrapping ErrUnknownGeneration", err)
}

// TestBackend_Upsert_MultiChunk_StoresAllChunks verifies a message that
// produces multiple chunks persists one row per chunk (not just the last,
// which the prior (generation_id, message_id) primary key collapsed to)
// while message_count and Stats.EmbeddingCount stay message-scoped.
func TestBackend_Upsert_MultiChunk_StoresAllChunks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	b, ctx, _ := newBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "m", 4, "")
	require.NoError(
		err, "CreateGeneration")

	require.NoError(
		b.Upsert(ctx, gen, []vector.Chunk{
			{MessageID: 1, ChunkIndex: 0, Vector: unitVec(4, 0)},
			{MessageID: 1, ChunkIndex: 1, Vector: unitVec(4, 1)},
		}), "Upsert")

	var rows int
	err = b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = $1 AND message_id = 1`,
		int64(gen)).Scan(&rows)
	require.NoError(
		err, "count chunk rows")

	assert.Equal(2, rows, "chunk rows (both chunks must be stored)")

	var msgCount int64
	err = b.db.QueryRowContext(ctx,
		`SELECT message_count FROM index_generations WHERE id = $1`, int64(gen)).Scan(&msgCount)
	require.NoError(
		err, "message_count")

	assert.Equal(int64(1), msgCount, "message_count (chunks of one message count once)")

	s, err := b.Stats(ctx, gen)
	require.NoError(
		err, "Stats")

	assert.Equal(int64(1), s.EmbeddingCount, "Stats.EmbeddingCount (distinct messages, not chunks)")
	assert.Positive(s.StorageBytes, "Stats.StorageBytes want > 0")
}

// TestBackend_Upsert_MultiChunk_ReplaceShrinks confirms re-upserting a
// message with fewer chunks removes the orphaned tail chunks rather than
// leaving them behind (chunk counts are not stable across re-embeds).
func TestBackend_Upsert_MultiChunk_ReplaceShrinks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	b, ctx, _ := newBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "m", 4, "")
	require.NoError(
		err, "CreateGeneration")

	require.NoError(
		b.Upsert(ctx, gen, []vector.Chunk{
			{MessageID: 1, ChunkIndex: 0, Vector: unitVec(4, 0)},
			{MessageID: 1, ChunkIndex: 1, Vector: unitVec(4, 1)},
			{MessageID: 1, ChunkIndex: 2, Vector: unitVec(4, 2)},
		}), "first Upsert")

	require.NoError(
		b.Upsert(ctx, gen, []vector.Chunk{
			{MessageID: 1, ChunkIndex: 0, Vector: unitVec(4, 3)},
		}), "second Upsert")

	var rows int
	err = b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = $1 AND message_id = 1`,
		int64(gen)).Scan(&rows)
	require.NoError(
		err, "count chunk rows")

	assert.Equal(1, rows, "chunk rows after shrink (orphan tail chunks must be removed)")

	var msgCount int64
	err = b.db.QueryRowContext(ctx,
		`SELECT message_count FROM index_generations WHERE id = $1`, int64(gen)).Scan(&msgCount)
	require.NoError(
		err, "message_count")

	assert.Equal(int64(1), msgCount, "message_count")
}

// TestBackend_Search_MultiChunk_OneHitPerMessage verifies Search returns
// at most one Hit per message (the best-scoring chunk) when a message has
// multiple chunks, so one message's chunks cannot crowd out other
// messages in the top-k.
func TestBackend_Search_MultiChunk_OneHitPerMessage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	b, ctx, db := newBackendForTest(t)
	_, err := db.ExecContext(ctx, `INSERT INTO messages (id) VALUES (2)`)
	require.NoError(
		err, "seed message 2")

	gen, err := b.CreateGeneration(ctx, "m", 4, "")
	require.NoError(
		err, "CreateGeneration")

	require.NoError(

		b.Upsert(ctx, gen, []vector.Chunk{
			{MessageID: 1, ChunkIndex: 0, Vector: unitVec(4, 0)},
			{MessageID: 1, ChunkIndex: 1, Vector: unitVec(4, 3)},
			{MessageID: 2, ChunkIndex: 0, Vector: unitVec(4, 1)},
		}), "Upsert")

	hits, err := b.Search(ctx, gen, unitVec(4, 0), 10, vector.Filter{})
	require.NoError(
		err, "Search")

	require.Len(hits, 2, "len(hits) want 2 (one per message)")

	seen := map[int64]int{}
	for _, h := range hits {
		seen[h.MessageID]++
	}
	for id, n := range seen {
		assert.Equal(1, n, "message %d returned %d times, want exactly 1", id, n)
	}
	assert.Equal(int64(1), hits[0].MessageID, "top hit want 1 (best chunk lies on the query axis)")
}

// TestBackend_Search_SubjectFilter_CaseInsensitive protects the
// case-insensitive subject filter: PostgreSQL LIKE is case-sensitive, so
// the backend lowercases both sides. A lowercase query term must match a
// mixed-case subject (a regression to plain LIKE would return nothing),
// and LIKE wildcards in the term must be matched literally (escaped).
func TestBackend_Search_SubjectFilter_CaseInsensitive(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	b, ctx, db := newBackendForTest(t)
	_, err := db.ExecContext(ctx, `INSERT INTO messages (id) VALUES (2), (3)`)
	require.NoError(
		err, "seed messages")

	subjects := map[int64]string{
		1: "Quarterly Invoice", // mixed case — exercises case-insensitivity
		2: "Team lunch",        // unrelated
		3: "50% discount code", // literal % — exercises wildcard escaping
	}
	for id, subj := range subjects {
		_, err = db.ExecContext(ctx,
			`UPDATE messages SET subject = $1 WHERE id = $2`, subj, id)
		require.NoErrorf(err, "set subject %d", id)
	}
	gen, err := b.CreateGeneration(ctx, "m", 4, "")
	require.NoError(
		err, "CreateGeneration")

	require.NoError(
		b.Upsert(ctx, gen, []vector.Chunk{
			{MessageID: 1, ChunkIndex: 0, Vector: unitVec(4, 0)},
			{MessageID: 2, ChunkIndex: 0, Vector: unitVec(4, 1)},
			{MessageID: 3, ChunkIndex: 0, Vector: unitVec(4, 2)},
		}), "Upsert")

	// Lowercase term must match the mixed-case "Quarterly Invoice".
	hits, err := b.Search(ctx, gen, unitVec(4, 0), 10,
		vector.Filter{SubjectSubstrings: []string{"invoice"}})
	require.NoError(
		err, "Search(invoice)")

	if assert.Len(hits, 1, "case-insensitive subject filter hits") {
		assert.Equal(int64(1), hits[0].MessageID, "want msg 1")
	}

	// A LIKE wildcard in the term is matched literally: "50%" matches the
	// literal "50% discount code" (msg 3) and nothing else.
	hits, err = b.Search(ctx, gen, unitVec(4, 2), 10,
		vector.Filter{SubjectSubstrings: []string{"50%"}})
	require.NoError(
		err, "Search(50%%)")

	if assert.Len(hits, 1, "escaped-wildcard subject filter hits") {
		assert.Equal(int64(3), hits[0].MessageID, "want msg 3")
	}
}
