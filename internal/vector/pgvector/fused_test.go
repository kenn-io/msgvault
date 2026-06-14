//go:build pgvector

package pgvector

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

// fusedFixture wires up a per-test schema with a richer messages
// table than backend_testhelpers_test.go's stripped-down version —
// fused search needs subject + search_fts + sent_at — and seeds a
// small synthetic corpus plus a matching set of embeddings.
type fusedFixture struct {
	b   *Backend
	ctx context.Context
	db  *sql.DB
	gen vector.GenerationID
}

func newFusedFixture(t *testing.T) *fusedFixture {
	t.Helper()
	db := openPGTestDB(t)
	// The shared minimal test schema lacks the inline search_fts column
	// the fused query reads, so add it here. In production this column
	// lives on the messages table from the main store schema.
	_, err := db.Exec(`ALTER TABLE messages ADD COLUMN IF NOT EXISTS search_fts TSVECTOR`)
	require.NoError(t, err, "add search_fts")
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS attachments (
        id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
        message_id BIGINT NOT NULL,
        filename TEXT
    )`)
	require.NoError(t, err, "create attachments")
	ctx := context.Background()
	b, err := Open(ctx, Options{DB: db, Dimension: 4})
	require.NoError(t, err, "Open")
	t.Cleanup(func() { _ = b.Close() })
	return &fusedFixture{b: b, ctx: ctx, db: db}
}

// seedMsg inserts a row in messages, optionally an attachment, and
// updates search_fts using the same 'simple' config the fused query
// uses on its side.
func (f *fusedFixture) seedMsg(t *testing.T, id int64, subject, body string, sourceID int64, sentAt time.Time, hasAttachment bool) {
	t.Helper()
	_, err := f.db.ExecContext(f.ctx,
		`INSERT INTO messages (id, source_id, subject, sent_at, has_attachments)
         VALUES ($1, $2, $3, $4, $5)`,
		id, sourceID, subject, sentAt, hasAttachment)
	require.NoErrorf(t, err, "insert msg %d", id)
	_, err = f.db.ExecContext(f.ctx,
		`UPDATE messages SET search_fts =
            setweight(to_tsvector('simple', COALESCE($2, '')), 'A') ||
            to_tsvector('simple', COALESCE($3, ''))
         WHERE id = $1`,
		id, subject, body)
	require.NoErrorf(t, err, "update search_fts %d", id)
	if hasAttachment {
		_, err := f.db.ExecContext(f.ctx,
			`INSERT INTO attachments (message_id, filename) VALUES ($1, 'doc.pdf')`,
			id)
		require.NoErrorf(t, err, "insert attachment %d", id)
	}
}

// embedAll creates a generation sized to the first vector and
// upserts every supplied chunk. The fixture-seeded message ids must
// exist before this is called.
func (f *fusedFixture) embedAll(t *testing.T, vecs map[int64][]float32) {
	t.Helper()
	gen, err := f.b.CreateGeneration(f.ctx, "m", 4, "")
	require.NoError(t, err, "CreateGeneration")
	f.gen = gen
	chunks := make([]vector.Chunk, 0, len(vecs))
	for id, v := range vecs {
		chunks = append(chunks, vector.Chunk{MessageID: id, Vector: v})
	}
	require.NoError(t, f.b.Upsert(f.ctx, gen, chunks), "Upsert")
	require.NoError(t, f.b.ActivateGeneration(f.ctx, gen, true), "Activate")
}

// embedChunks creates a generation sized to the first chunk's vector
// and upserts the supplied chunks verbatim, preserving each chunk's
// ChunkIndex. Unlike embedAll (one vector per message), this lets a
// single message carry several chunks so multi-chunk dedup can be
// exercised. The fixture-seeded message ids must exist beforehand.
func (f *fusedFixture) embedChunks(t *testing.T, chunks []vector.Chunk) {
	t.Helper()
	require.NotEmpty(t, chunks, "embedChunks: no chunks supplied")
	gen, err := f.b.CreateGeneration(f.ctx, "m", len(chunks[0].Vector), "")
	require.NoError(t, err, "CreateGeneration")
	f.gen = gen
	require.NoError(t, f.b.Upsert(f.ctx, gen, chunks), "Upsert")
	require.NoError(t, f.b.ActivateGeneration(f.ctx, gen, true), "Activate")
}

// seedThree wires up a 3-message corpus with distinct vectors and
// distinct keyword content so each test below can mix-and-match
// signal/filter combinations on the same data shape.
func seedThree(t *testing.T) *fusedFixture {
	t.Helper()
	f := newFusedFixture(t)
	base := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	f.seedMsg(t, 1, "alpha quantum project update", "discussing the quantum roadmap", 10, base, false)
	f.seedMsg(t, 2, "beta vector indexing notes", "notes about hybrid search and ranking", 20, base.AddDate(0, 0, 1), true)
	f.seedMsg(t, 3, "gamma project retrospective", "retro covering the quantum milestone", 10, base.AddDate(0, 0, 2), false)
	f.embedAll(t, map[int64][]float32{
		1: unitVec(4, 0),
		2: unitVec(4, 1),
		3: unitVec(4, 2),
	})
	return f
}

func TestFusedSearch_FTSOnly(t *testing.T) {
	f := seedThree(t)
	hits, saturated, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		FTSQuery:   "quantum",
		Generation: f.gen,
		KPerSignal: 10,
		Limit:      10,
		RRFK:       60,
	})
	require.NoError(t, err, "FusedSearch")
	assert.False(t, saturated, "saturated should be false (pool size 2 < KPerSignal 10)")
	require.Len(t, hits, 2, "hits should be msgs 1 and 3 (mention 'quantum'); hits=%+v", hits)
	seen := map[int64]bool{}
	for i, h := range hits {
		seen[h.MessageID] = true
		assert.Truef(t, math.IsNaN(h.VectorScore), "hit[%d].VectorScore = %v, want NaN (FTS-only)", i, h.VectorScore)
		assert.Falsef(t, math.IsNaN(h.BM25Score), "hit[%d].BM25Score = NaN, want a number (FTS-only)", i)
		assert.Greaterf(t, h.RRFScore, 0.0, "hit[%d].RRFScore = %v, want > 0", i, h.RRFScore)
	}
	assert.True(t, seen[1] && seen[3], "expected msgs 1 and 3, got %v", seen)
	// Hits ordered by RRFScore descending.
	for i := 1; i < len(hits); i++ {
		assert.GreaterOrEqualf(t, hits[i-1].RRFScore, hits[i].RRFScore,
			"RRF not descending at %d: %v then %v", i, hits[i-1].RRFScore, hits[i].RRFScore)
	}
}

func TestFusedSearch_ANNOnly(t *testing.T) {
	f := seedThree(t)
	hits, saturated, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		QueryVec:   unitVec(4, 0),
		Generation: f.gen,
		KPerSignal: 10,
		Limit:      10,
		RRFK:       60,
	})
	require.NoError(t, err, "FusedSearch")
	assert.False(t, saturated, "saturated should be false")
	require.NotEmpty(t, hits, "expected hits, got none")
	assert.Equal(t, int64(1), hits[0].MessageID, "top hit should be 1 (query points along axis 0)")
	for i, h := range hits {
		assert.Truef(t, math.IsNaN(h.BM25Score), "hit[%d].BM25Score = %v, want NaN (ANN-only)", i, h.BM25Score)
		assert.Falsef(t, math.IsNaN(h.VectorScore), "hit[%d].VectorScore = NaN, want a number (ANN-only)", i)
	}
}

func TestFusedSearch_Hybrid(t *testing.T) {
	f := seedThree(t)
	hits, saturated, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		FTSQuery:   "quantum",
		QueryVec:   unitVec(4, 1), // points at msg 2 (no 'quantum')
		Generation: f.gen,
		KPerSignal: 10,
		Limit:      10,
		RRFK:       60,
	})
	require.NoError(t, err, "FusedSearch")
	assert.False(t, saturated, "saturated should be false")
	// Expect: msg 2 via ANN; msgs 1 and 3 via FTS — union of 3.
	require.Len(t, hits, 3, "hits should be the union of 3; hits=%+v", hits)
	for i := 1; i < len(hits); i++ {
		assert.GreaterOrEqualf(t, hits[i-1].RRFScore, hits[i].RRFScore,
			"RRF not descending at %d: %v then %v", i, hits[i-1].RRFScore, hits[i].RRFScore)
	}
}

// TestFusedSearch_MultiChunk_OneHitPerMessage regression-guards the
// ANN pool's per-message dedup. The ann_pool CTE collapses chunks via
// GROUP BY message_id + MIN(distance) (fused.go ~line 114-123). Without
// that collapse a multi-chunk message yields one ann_pool row per chunk;
// the FULL OUTER JOIN ... USING (message_id) in the fused CTE then emits
// duplicate message_id rows and the per-chunk ranks distort RRF.
//
// Setup mirrors backend_test.go's TestBackend_Search_MultiChunk_OneHitPerMessage:
// msg 1 has TWO chunks (chunk_index 0 close to the query along axis 0,
// chunk_index 1 far along axis 2); msg 2 is a single-chunk competitor on
// axis 1. The query points along axis 0.
func TestFusedSearch_MultiChunk_OneHitPerMessage(t *testing.T) {
	f := newFusedFixture(t)
	base := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	// Both messages match the FTS query so the hybrid path joins ann/fts;
	// distinct subjects keep the corpus realistic.
	f.seedMsg(t, 1, "alpha report rollup", "the report covers everything", 10, base, false)
	f.seedMsg(t, 2, "beta report digest", "another report entirely", 20, base, false)
	f.embedChunks(t, []vector.Chunk{
		{MessageID: 1, ChunkIndex: 0, Vector: unitVec(4, 0)}, // CLOSE to query → distance 0
		{MessageID: 1, ChunkIndex: 1, Vector: unitVec(4, 2)}, // FAR from query  → distance 1
		{MessageID: 2, ChunkIndex: 0, Vector: unitVec(4, 1)}, // single-chunk competitor
	})

	// ANN-only: isolates the ann_pool dedup from any FTS contribution.
	t.Run("ann_only", func(t *testing.T) {
		hits, _, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
			QueryVec:   unitVec(4, 0),
			Generation: f.gen,
			KPerSignal: 10,
			Limit:      10,
			RRFK:       60,
		})
		require.NoError(t, err, "FusedSearch")
		// Exactly two rows — one per message, no duplicate for msg 1's
		// second chunk.
		require.Len(t, hits, 2, "want one row per message; hits=%+v", hits)
		counts := map[int64]int{}
		for _, h := range hits {
			counts[h.MessageID]++
		}
		assert.Equal(t, 1, counts[1], "msg 1 must appear exactly once (its two chunks collapse)")
		assert.Equal(t, 1, counts[2], "msg 2 must appear exactly once")
		// The close chunk (distance 0) wins the MIN, so msg 1's
		// VectorScore (= 1 - distance) is ~1.0 and it ranks first.
		assert.Equal(t, int64(1), hits[0].MessageID, "msg 1 wins on its close chunk")
		var msg1 vector.FusedHit
		for _, h := range hits {
			if h.MessageID == 1 {
				msg1 = h
			}
		}
		assert.InDelta(t, 1.0, msg1.VectorScore, 1e-6,
			"msg 1 effective VectorScore must reflect MIN distance (close chunk), got %v", msg1.VectorScore)
	})

	// Hybrid: the far chunk must not earn msg 1 a second ann_ranked row
	// that would give it extra RRF weight and crowd out the competitor.
	t.Run("hybrid", func(t *testing.T) {
		hits, _, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
			FTSQuery:   "report",
			QueryVec:   unitVec(4, 0),
			Generation: f.gen,
			KPerSignal: 10,
			Limit:      10,
			RRFK:       60,
		})
		require.NoError(t, err, "FusedSearch")
		require.Len(t, hits, 2, "want exactly msgs 1 and 2, each once; hits=%+v", hits)
		counts := map[int64]int{}
		for _, h := range hits {
			counts[h.MessageID]++
		}
		assert.Equal(t, 1, counts[1], "msg 1 must appear exactly once in hybrid mode")
		assert.Equal(t, 1, counts[2], "competitor msg 2 must not be crowded out")
		// RRF descending invariant holds.
		for i := 1; i < len(hits); i++ {
			assert.GreaterOrEqualf(t, hits[i-1].RRFScore, hits[i].RRFScore,
				"RRF not descending at %d: %v then %v", i, hits[i-1].RRFScore, hits[i].RRFScore)
		}
	})
}

func TestFusedSearch_Saturated(t *testing.T) {
	f := seedThree(t)
	hits, saturated, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		FTSQuery:   "quantum",
		Generation: f.gen,
		KPerSignal: 1, // smaller than the 2-row FTS pool → saturates
		Limit:      10,
		RRFK:       60,
	})
	require.NoError(t, err, "FusedSearch")
	assert.True(t, saturated, "saturated should be true (KPerSignal=1, pool=2)")
	assert.Len(t, hits, 1, "hits should be trimmed to KPerSignal")
}

func TestFusedSearch_FilterBySource(t *testing.T) {
	f := seedThree(t)
	hits, saturated, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		FTSQuery:   "quantum",
		Generation: f.gen,
		KPerSignal: 10,
		Limit:      10,
		RRFK:       60,
		Filter:     vector.Filter{SourceIDs: []int64{20}},
	})
	require.NoError(t, err, "FusedSearch")
	// SourceIDs={20} only allows msg 2 through, which doesn't match 'quantum'.
	assert.Empty(t, hits, "want empty (source 20 has no quantum match)")
	// Empty result drives the saturation fallback: with an empty pool
	// (0 ≤ KPerSignal=10) there is no overflow, so saturated must be false.
	assert.False(t, saturated, "empty filtered pool cannot saturate")

	hits, saturated, err = f.b.FusedSearch(f.ctx, vector.FusedRequest{
		FTSQuery:   "quantum",
		Generation: f.gen,
		KPerSignal: 10,
		Limit:      10,
		RRFK:       60,
		Filter:     vector.Filter{SourceIDs: []int64{10}},
	})
	require.NoError(t, err, "FusedSearch (source 10)")
	assert.Len(t, hits, 2, "want msgs 1+3 in source 10 that match quantum")
	assert.False(t, saturated, "pool size 2 < KPerSignal 10 must not saturate")
}

func TestFusedSearch_FilterByDateRange(t *testing.T) {
	f := seedThree(t)
	after := time.Date(2025, 1, 16, 0, 0, 0, 0, time.UTC) // exclude msg 1
	hits, _, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		FTSQuery:   "quantum",
		Generation: f.gen,
		KPerSignal: 10,
		Limit:      10,
		RRFK:       60,
		Filter:     vector.Filter{After: &after},
	})
	require.NoError(t, err, "FusedSearch")
	// After 2025-01-16: msg 2 (2025-01-16) and msg 3 (2025-01-17) survive
	// the filter, but only msg 3 mentions 'quantum'.
	require.Len(t, hits, 1, "want exactly msg 3")
	assert.Equal(t, int64(3), hits[0].MessageID, "want exactly msg 3")
}

func TestFusedSearch_FilterByLabel(t *testing.T) {
	f := seedThree(t)
	// Tag msg 3 with label_id 42 only.
	_, err := f.db.ExecContext(f.ctx,
		`INSERT INTO message_labels (message_id, label_id) VALUES (3, 42)`)
	require.NoError(t, err, "insert label")
	hits, _, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		FTSQuery:   "quantum",
		Generation: f.gen,
		KPerSignal: 10,
		Limit:      10,
		RRFK:       60,
		Filter:     vector.Filter{LabelGroups: [][]int64{{42}}},
	})
	require.NoError(t, err, "FusedSearch")
	require.Len(t, hits, 1, "want exactly msg 3 (labeled 42)")
	assert.Equal(t, int64(3), hits[0].MessageID, "want exactly msg 3 (labeled 42)")
}

func TestFusedSearch_FilterBySender(t *testing.T) {
	f := seedThree(t)
	// Add a 'from' recipient row for msg 1 only, participant_id=99.
	_, err := f.db.ExecContext(f.ctx,
		`INSERT INTO message_recipients (message_id, recipient_type, participant_id)
         VALUES (1, 'from', 99)`)
	require.NoError(t, err, "insert from-recipient")
	hits, _, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		FTSQuery:   "quantum",
		Generation: f.gen,
		KPerSignal: 10,
		Limit:      10,
		RRFK:       60,
		Filter:     vector.Filter{SenderGroups: [][]int64{{99}}},
	})
	require.NoError(t, err, "FusedSearch")
	require.Len(t, hits, 1, "want exactly msg 1 (only from=99)")
	assert.Equal(t, int64(1), hits[0].MessageID, "want exactly msg 1 (only from=99)")
}

// TestFusedSearch_FilterBySender_NoMatch verifies the negative parity: a
// SenderGroups filter whose participant_id is NOT linked as a from-recipient
// on any message correctly returns zero hits. This guards against the filter
// accidentally degrading to an unfiltered search when the participant is absent.
func TestFusedSearch_FilterBySender_NoMatch(t *testing.T) {
	f := seedThree(t)
	// participant_id=777 is not a from-recipient on any of the three seeded
	// messages — the filter should exclude every candidate.
	hits, _, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		FTSQuery:   "quantum",
		Generation: f.gen,
		KPerSignal: 10,
		Limit:      10,
		RRFK:       60,
		Filter:     vector.Filter{SenderGroups: [][]int64{{777}}},
	})
	require.NoError(t, err, "FusedSearch")
	assert.Empty(t, hits, "expected zero hits: participant 777 is not a from-recipient")
}

func TestFusedSearch_HasAttachment(t *testing.T) {
	f := seedThree(t)
	yes := true
	hits, _, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		QueryVec:   unitVec(4, 1),
		Generation: f.gen,
		KPerSignal: 10,
		Limit:      10,
		RRFK:       60,
		Filter:     vector.Filter{HasAttachment: &yes},
	})
	require.NoError(t, err, "FusedSearch")
	// Only msg 2 has an attachment.
	require.Len(t, hits, 1, "want exactly msg 2")
	assert.Equal(t, int64(2), hits[0].MessageID, "want exactly msg 2")
}

func TestFusedSearch_RejectsEmptyRequest(t *testing.T) {
	f := seedThree(t)
	_, _, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		Generation: f.gen,
		KPerSignal: 10,
		Limit:      10,
		RRFK:       60,
	})
	assert.Error(t, err, "expected error for empty fts/vec")
}

func TestFusedSearch_UnknownGeneration(t *testing.T) {
	f := newFusedFixture(t)
	_, _, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		FTSQuery:   "anything",
		Generation: 999,
		KPerSignal: 10,
		Limit:      10,
		RRFK:       60,
	})
	assert.Error(t, err, "expected error for unknown generation")
}

func TestFusedSearch_DimensionMismatch(t *testing.T) {
	f := seedThree(t)
	_, _, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		QueryVec:   []float32{1, 2, 3}, // 3-dim, gen is 4-dim
		Generation: f.gen,
		KPerSignal: 10,
		Limit:      10,
		RRFK:       60,
	})
	assert.Error(t, err, "expected dimension-mismatch error")
}

// TestFusedSearch_SubjectBoost asserts that an entry with a matching
// subject substring is re-ranked above one that would otherwise win
// on RRF alone. Msg 2 (vector match) has the boosted subject token
// "vector"; msgs 1 and 3 win on FTS but don't contain "vector".
func TestFusedSearch_SubjectBoost(t *testing.T) {
	f := seedThree(t)
	hits, _, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		FTSQuery:     "quantum",
		QueryVec:     unitVec(4, 1),
		Generation:   f.gen,
		KPerSignal:   10,
		Limit:        10,
		RRFK:         60,
		SubjectBoost: 50.0,
		SubjectTerms: []string{"vector"},
	})
	require.NoError(t, err, "FusedSearch")
	require.NotEmpty(t, hits, "expected hits")
	assert.Equal(t, int64(2), hits[0].MessageID, "top hit should be msg 2 (subject boosted)")
	assert.True(t, hits[0].SubjectBoosted, "hit[0].SubjectBoosted should be true")
}

// TestFusedSearch_SkipsDeletedMessages confirms the live-message
// predicate is applied — a soft-deleted message must not appear in
// either signal's pool.
func TestFusedSearch_SkipsDeletedMessages(t *testing.T) {
	f := seedThree(t)
	_, err := f.db.ExecContext(f.ctx,
		`UPDATE messages SET deleted_from_source_at = NOW() WHERE id = 1`)
	require.NoError(t, err, "soft-delete")
	hits, _, err := f.b.FusedSearch(f.ctx, vector.FusedRequest{
		FTSQuery:   "quantum",
		QueryVec:   unitVec(4, 0),
		Generation: f.gen,
		KPerSignal: 10,
		Limit:      10,
		RRFK:       60,
	})
	require.NoError(t, err, "FusedSearch")
	for _, h := range hits {
		assert.NotEqualf(t, int64(1), h.MessageID, "hits include soft-deleted msg 1: %+v", h)
	}
}

// TestFusedSearch_FilterClausesUnused — sanity-check the static
// SQL-formatting code path: build an applyFilterClauses fragment by
// hand and assert it begins with a leading " AND " (so it can be
// safely concatenated after a WHERE predicate). The clause must
// reference each placeholder it claims to bind.
func TestFusedSearch_FilterClausesUnused(t *testing.T) {
	var args []any
	bind := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	f := vector.Filter{SourceIDs: []int64{1, 2}}
	got := applyFilterClauses(f, bind)
	assert.Len(t, args, 1, "args bound should be 1")
	require.GreaterOrEqual(t, len(got), 5, "clauses = %q, want leading ' AND '", got)
	assert.Equal(t, " AND ", got[:5], "clauses = %q, want leading ' AND '", got)
}
