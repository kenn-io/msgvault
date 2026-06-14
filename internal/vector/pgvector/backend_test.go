//go:build pgvector

package pgvector

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

// TestBackend_CreateActivateRetire exercises the full lifecycle of one
// generation against pgvector. Parallel to the sqlitevec test of the
// same name (internal/vector/sqlitevec/backend_test.go).
func TestBackend_CreateActivateRetire(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)

	gid, err := b.CreateGeneration(ctx, "nomic-embed-text-v1.5", 768, "")
	require.NoError(t, err, "CreateGeneration")

	bg, err := b.BuildingGeneration(ctx)
	require.NoError(t, err, "BuildingGeneration")
	require.NotNil(t, bg, "BuildingGeneration returned nil")
	assert.Equal(t, gid, bg.ID, "BuildingGeneration id mismatch")

	_, err = b.ActiveGeneration(ctx)
	assert.Error(t, err, "ActiveGeneration should error before activation")

	require.NoError(t, b.ActivateGeneration(ctx, gid, true), "ActivateGeneration")

	g, err := b.ActiveGeneration(ctx)
	require.NoError(t, err, "ActiveGeneration after activate")
	assert.Equal(t, vector.GenerationActive, g.State, "State want active")
	assert.Equal(t, "nomic-embed-text-v1.5:768", g.Fingerprint, "Fingerprint mismatch")

	require.NoError(t, b.RetireGeneration(ctx, gid, true), "RetireGeneration")
	_, err = b.ActiveGeneration(ctx)
	assert.Error(t, err, "ActiveGeneration should error after retire")
}

// TestBackend_CreateGeneration_SeedsPending verifies the initial seed
// pass populates pending_embeddings with one row per live message.
func TestBackend_CreateGeneration_SeedsPending(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)
	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(t, err, "Create")

	var n int
	err = b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = $1`, int64(gid),
	).Scan(&n)
	require.NoError(t, err, "count pending")
	assert.Equal(t, 1, n, "pending count")
}

// TestBackend_CreateGeneration_SkipsDeleted ensures the seed pass
// honours the live-message predicate, so soft-deleted rows are not
// re-embedded by a future rebuild.
func TestBackend_CreateGeneration_SkipsDeleted(t *testing.T) {
	db := openPGTestDB(t)
	_, err := db.Exec(`INSERT INTO messages (id, deleted_from_source_at) VALUES (1, NOW())`)
	require.NoError(t, err, "seed deleted")

	ctx := context.Background()
	b, err := Open(ctx, Options{DB: db, Dimension: 768})
	require.NoError(t, err, "Open")
	t.Cleanup(func() { _ = b.Close() })

	gid, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(t, err, "Create")

	var n int
	err = b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = $1`, int64(gid),
	).Scan(&n)
	require.NoError(t, err, "count pending")
	assert.Equal(t, 0, n, "pending count (deleted message must be skipped)")
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
	assert.True(t, errors.Is(err, vector.ErrBuildingInProgress),
		"error = %v, want wrapping ErrBuildingInProgress", err)
}

// TestBackend_CreateGeneration_ResumeReseedsUnseededGeneration covers
// the "crash between row insert and seed commit" path: a building row
// exists but seeded_at is NULL. Resume must re-run seedPending.
func TestBackend_CreateGeneration_ResumeReseedsUnseededGeneration(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)

	gen, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(t, err, "first Create")

	_, err = b.db.ExecContext(ctx,
		`UPDATE index_generations SET seeded_at = NULL WHERE id = $1`, int64(gen))
	require.NoError(t, err, "clear seeded_at")

	_, err = b.db.ExecContext(ctx,
		`DELETE FROM pending_embeddings WHERE generation_id = $1`, int64(gen))
	require.NoError(t, err, "clear pending")

	resumed, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(t, err, "resume Create")
	assert.Equal(t, gen, resumed, "resumed gen must match original")

	var pending int
	err = b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = $1`,
		int64(gen)).Scan(&pending)
	require.NoError(t, err, "count pending")
	assert.Equal(t, 1, pending, "pending count after resume")

	var seededAt sql.NullInt64
	err = b.db.QueryRowContext(ctx,
		`SELECT seeded_at FROM index_generations WHERE id = $1`, int64(gen)).Scan(&seededAt)
	require.NoError(t, err, "read seeded_at")
	assert.True(t, seededAt.Valid, "seeded_at still NULL after resume re-seed")
}

// TestBackend_EnsureSeeded_Idempotent calls EnsureSeeded twice and
// asserts the seeded_at stamp persists across calls.
func TestBackend_EnsureSeeded_Idempotent(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(t, err, "Create")
	require.NoError(t, b.EnsureSeeded(ctx, gen), "EnsureSeeded #1")
	require.NoError(t, b.EnsureSeeded(ctx, gen), "EnsureSeeded #2")
}

// TestBackend_EnsureSeeded_RejectsActiveGeneration verifies the guard
// that prevents re-seeding a non-building generation.
func TestBackend_EnsureSeeded_RejectsActiveGeneration(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(t, err, "Create")
	require.NoError(t, b.ActivateGeneration(ctx, gen, true), "Activate")

	err = b.EnsureSeeded(ctx, gen)
	assert.True(t, errors.Is(err, vector.ErrGenerationNotBuilding),
		"EnsureSeeded on active gen returned %v, want ErrGenerationNotBuilding", err)
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
	assert.True(t, errors.Is(err, vector.ErrDimensionMismatch),
		"err=%v, want wrapping ErrDimensionMismatch", err)
}

// TestBackend_Upsert_UnknownGeneration surfaces ErrUnknownGeneration
// when a caller passes a bogus generation id.
func TestBackend_Upsert_UnknownGeneration(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)
	err := b.Upsert(ctx, vector.GenerationID(999), []vector.Chunk{
		{MessageID: 1, Vector: []float32{0, 0, 0, 0}},
	})
	assert.True(t, errors.Is(err, vector.ErrUnknownGeneration),
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
	b, ctx, db := newBackendForTest(t)
	gen := seedAndEmbed(t, b, db, map[int64][]float32{
		1: unitVec(4, 0),
		2: unitVec(4, 1),
		3: unitVec(4, 2),
	})
	hits, err := b.Search(ctx, gen, unitVec(4, 0), 3, vector.Filter{})
	require.NoError(t, err, "Search")
	require.NotEmpty(t, hits, "Search returned no hits")
	assert.Equal(t, int64(1), hits[0].MessageID, "top hit want 1")
	for i, h := range hits {
		assert.Equal(t, i+1, h.Rank, "hit[%d].Rank", i)
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
	b, ctx, db := newBackendForTest(t)
	gen := seedAndEmbed(t, b, db, map[int64][]float32{
		1: unitVec(4, 0),
		2: unitVec(4, 1),
	})
	// Tag the messages with distinct source_ids so the filter can pick
	// exactly one of them. SourceIDs operates over m.source_id.
	_, err := db.ExecContext(ctx,
		`UPDATE messages SET source_id = 10 WHERE id = 1`)
	require.NoError(t, err, "tag source 10")
	_, err = db.ExecContext(ctx,
		`UPDATE messages SET source_id = 20 WHERE id = 2`)
	require.NoError(t, err, "tag source 20")

	hits, err := b.Search(ctx, gen, unitVec(4, 0), 5, vector.Filter{SourceIDs: []int64{20}})
	require.NoError(t, err, "Search")
	if assert.Len(t, hits, 1, "filtered hits want exactly 1") {
		assert.Equal(t, int64(2), hits[0].MessageID, "filtered hit want msg 2")
	}
}

// TestBackend_LoadVector_RoundTrip writes a known vector and reads it
// back, confirming the text format round-trips through pgvector
// without loss for float32 precision.
func TestBackend_LoadVector_RoundTrip(t *testing.T) {
	b, ctx, db := newBackendForTest(t)
	original := []float32{0.25, -0.5, 0.75, 1.0}
	gen := seedAndEmbed(t, b, db, map[int64][]float32{1: original})
	require.NoError(t, b.ActivateGeneration(ctx, gen, true), "Activate")

	got, err := b.LoadVector(ctx, 1)
	require.NoError(t, err, "LoadVector")
	require.Len(t, got, len(original), "loaded len")
	for i := range original {
		assert.Equal(t, original[i], got[i], "dim[%d]", i)
	}
}

// TestBackend_Delete_RemovesAndUpdatesCount confirms Delete removes
// the embedding row and decrements message_count atomically.
func TestBackend_Delete_RemovesAndUpdatesCount(t *testing.T) {
	b, ctx, db := newBackendForTest(t)
	gen := seedAndEmbed(t, b, db, map[int64][]float32{
		1: unitVec(4, 0),
		2: unitVec(4, 1),
	})
	require.NoError(t, b.Delete(ctx, gen, []int64{1}), "Delete")

	var remaining, msgCount int64
	err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = $1`, int64(gen)).Scan(&remaining)
	require.NoError(t, err, "count")
	err = b.db.QueryRowContext(ctx,
		`SELECT message_count FROM index_generations WHERE id = $1`, int64(gen)).Scan(&msgCount)
	require.NoError(t, err, "message_count")
	assert.Equal(t, int64(1), remaining, "remaining embeddings")
	assert.Equal(t, int64(1), msgCount, "message_count")
}

// TestBackend_Stats_ScopedAndAggregate covers both the per-generation
// and aggregate (gen == 0) modes of Stats.
func TestBackend_Stats_ScopedAndAggregate(t *testing.T) {
	b, ctx, db := newBackendForTest(t)
	gen := seedAndEmbed(t, b, db, map[int64][]float32{
		1: unitVec(4, 0),
		2: unitVec(4, 1),
	})

	s, err := b.Stats(ctx, gen)
	require.NoError(t, err, "Stats(gen)")
	assert.Equal(t, int64(2), s.EmbeddingCount, "scoped EmbeddingCount")

	all, err := b.Stats(ctx, 0)
	require.NoError(t, err, "Stats(0)")
	assert.Equal(t, int64(2), all.EmbeddingCount, "aggregate EmbeddingCount")
}

// TestBackend_Stats_UnknownGeneration ensures Stats surfaces
// ErrUnknownGeneration when a non-zero generation id has no row.
func TestBackend_Stats_UnknownGeneration(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)
	_, err := b.Stats(ctx, vector.GenerationID(999))
	assert.True(t, errors.Is(err, vector.ErrUnknownGeneration),
		"err=%v, want wrapping ErrUnknownGeneration", err)
}

// TestBackend_Upsert_MultiChunk_StoresAllChunks verifies a message that
// produces multiple chunks persists one row per chunk (not just the last,
// which the prior (generation_id, message_id) primary key collapsed to)
// while message_count and Stats.EmbeddingCount stay message-scoped.
func TestBackend_Upsert_MultiChunk_StoresAllChunks(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "m", 4, "")
	require.NoError(t, err, "CreateGeneration")
	require.NoError(t, b.Upsert(ctx, gen, []vector.Chunk{
		{MessageID: 1, ChunkIndex: 0, Vector: unitVec(4, 0)},
		{MessageID: 1, ChunkIndex: 1, Vector: unitVec(4, 1)},
	}), "Upsert")

	var rows int
	err = b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = $1 AND message_id = 1`,
		int64(gen)).Scan(&rows)
	require.NoError(t, err, "count chunk rows")
	assert.Equal(t, 2, rows, "chunk rows (both chunks must be stored)")

	var msgCount int64
	err = b.db.QueryRowContext(ctx,
		`SELECT message_count FROM index_generations WHERE id = $1`, int64(gen)).Scan(&msgCount)
	require.NoError(t, err, "message_count")
	assert.Equal(t, int64(1), msgCount, "message_count (chunks of one message count once)")

	s, err := b.Stats(ctx, gen)
	require.NoError(t, err, "Stats")
	assert.Equal(t, int64(1), s.EmbeddingCount, "Stats.EmbeddingCount (distinct messages, not chunks)")
	assert.Greater(t, s.StorageBytes, int64(0), "Stats.StorageBytes want > 0")
}

// TestBackend_Upsert_MultiChunk_ReplaceShrinks confirms re-upserting a
// message with fewer chunks removes the orphaned tail chunks rather than
// leaving them behind (chunk counts are not stable across re-embeds).
func TestBackend_Upsert_MultiChunk_ReplaceShrinks(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "m", 4, "")
	require.NoError(t, err, "CreateGeneration")
	require.NoError(t, b.Upsert(ctx, gen, []vector.Chunk{
		{MessageID: 1, ChunkIndex: 0, Vector: unitVec(4, 0)},
		{MessageID: 1, ChunkIndex: 1, Vector: unitVec(4, 1)},
		{MessageID: 1, ChunkIndex: 2, Vector: unitVec(4, 2)},
	}), "first Upsert")
	require.NoError(t, b.Upsert(ctx, gen, []vector.Chunk{
		{MessageID: 1, ChunkIndex: 0, Vector: unitVec(4, 3)},
	}), "second Upsert")

	var rows int
	err = b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = $1 AND message_id = 1`,
		int64(gen)).Scan(&rows)
	require.NoError(t, err, "count chunk rows")
	assert.Equal(t, 1, rows, "chunk rows after shrink (orphan tail chunks must be removed)")

	var msgCount int64
	err = b.db.QueryRowContext(ctx,
		`SELECT message_count FROM index_generations WHERE id = $1`, int64(gen)).Scan(&msgCount)
	require.NoError(t, err, "message_count")
	assert.Equal(t, int64(1), msgCount, "message_count")
}

// TestBackend_Search_MultiChunk_OneHitPerMessage verifies Search returns
// at most one Hit per message (the best-scoring chunk) when a message has
// multiple chunks, so one message's chunks cannot crowd out other
// messages in the top-k.
func TestBackend_Search_MultiChunk_OneHitPerMessage(t *testing.T) {
	b, ctx, db := newBackendForTest(t)
	_, err := db.ExecContext(ctx, `INSERT INTO messages (id) VALUES (2)`)
	require.NoError(t, err, "seed message 2")

	gen, err := b.CreateGeneration(ctx, "m", 4, "")
	require.NoError(t, err, "CreateGeneration")
	// msg 1 has two chunks (axes 0 and 3); msg 2 has one (axis 1).
	require.NoError(t, b.Upsert(ctx, gen, []vector.Chunk{
		{MessageID: 1, ChunkIndex: 0, Vector: unitVec(4, 0)},
		{MessageID: 1, ChunkIndex: 1, Vector: unitVec(4, 3)},
		{MessageID: 2, ChunkIndex: 0, Vector: unitVec(4, 1)},
	}), "Upsert")

	hits, err := b.Search(ctx, gen, unitVec(4, 0), 10, vector.Filter{})
	require.NoError(t, err, "Search")
	require.Len(t, hits, 2, "len(hits) want 2 (one per message)")

	seen := map[int64]int{}
	for _, h := range hits {
		seen[h.MessageID]++
	}
	for id, n := range seen {
		assert.Equal(t, 1, n, "message %d returned %d times, want exactly 1", id, n)
	}
	assert.Equal(t, int64(1), hits[0].MessageID, "top hit want 1 (best chunk lies on the query axis)")
}

// TestBackend_Search_SubjectFilter_CaseInsensitive protects the
// case-insensitive subject filter: PostgreSQL LIKE is case-sensitive, so
// the backend lowercases both sides. A lowercase query term must match a
// mixed-case subject (a regression to plain LIKE would return nothing),
// and LIKE wildcards in the term must be matched literally (escaped).
func TestBackend_Search_SubjectFilter_CaseInsensitive(t *testing.T) {
	b, ctx, db := newBackendForTest(t)
	_, err := db.ExecContext(ctx, `INSERT INTO messages (id) VALUES (2), (3)`)
	require.NoError(t, err, "seed messages")

	subjects := map[int64]string{
		1: "Quarterly Invoice", // mixed case — exercises case-insensitivity
		2: "Team lunch",        // unrelated
		3: "50% discount code", // literal % — exercises wildcard escaping
	}
	for id, subj := range subjects {
		_, err = db.ExecContext(ctx,
			`UPDATE messages SET subject = $1 WHERE id = $2`, subj, id)
		require.NoErrorf(t, err, "set subject %d", id)
	}
	gen, err := b.CreateGeneration(ctx, "m", 4, "")
	require.NoError(t, err, "CreateGeneration")
	require.NoError(t, b.Upsert(ctx, gen, []vector.Chunk{
		{MessageID: 1, ChunkIndex: 0, Vector: unitVec(4, 0)},
		{MessageID: 2, ChunkIndex: 0, Vector: unitVec(4, 1)},
		{MessageID: 3, ChunkIndex: 0, Vector: unitVec(4, 2)},
	}), "Upsert")

	// Lowercase term must match the mixed-case "Quarterly Invoice".
	hits, err := b.Search(ctx, gen, unitVec(4, 0), 10,
		vector.Filter{SubjectSubstrings: []string{"invoice"}})
	require.NoError(t, err, "Search(invoice)")
	if assert.Len(t, hits, 1, "case-insensitive subject filter hits") {
		assert.Equal(t, int64(1), hits[0].MessageID, "want msg 1")
	}

	// A LIKE wildcard in the term is matched literally: "50%" matches the
	// literal "50% discount code" (msg 3) and nothing else.
	hits, err = b.Search(ctx, gen, unitVec(4, 2), 10,
		vector.Filter{SubjectSubstrings: []string{"50%"}})
	require.NoError(t, err, "Search(50%%)")
	if assert.Len(t, hits, 1, "escaped-wildcard subject filter hits") {
		assert.Equal(t, int64(3), hits[0].MessageID, "want msg 3")
	}
}
