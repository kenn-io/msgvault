package hybrid

import (
	"math"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/vector"
)

func TestFuse_BothSignalsContribute(t *testing.T) {
	assert := assertpkg.New(t)
	bm25 := []vector.Hit{{MessageID: 1, Rank: 1}, {MessageID: 2, Rank: 2}, {MessageID: 3, Rank: 3}}
	vec := []vector.Hit{{MessageID: 2, Rank: 1}, {MessageID: 4, Rank: 2}, {MessageID: 1, Rank: 3}}
	out := Fuse(bm25, vec, 60, 1.0, nil, nil)
	requirepkg.Len(t, out, 4)
	// Msg 2: BM25 rank 2 (1/62) + vec rank 1 (1/61) ≈ 0.03251 → highest.
	// Msg 1: BM25 rank 1 (1/61) + vec rank 3 (1/63) ≈ 0.03226.
	assert.Equal(int64(2), out[0].MessageID, "top")
	// Msg 3 is only in BM25; msg 4 only in vec. Both should have one NaN.
	for _, h := range out {
		switch h.MessageID {
		case 3:
			assert.False(math.IsNaN(h.BM25Score), "msg 3 BM25 should be non-NaN (or zero; Score=0)")
			assert.True(math.IsNaN(h.VectorScore), "msg 3 VectorScore should be NaN, got %v", h.VectorScore)
		case 4:
			assert.True(math.IsNaN(h.BM25Score), "msg 4 BM25Score should be NaN, got %v", h.BM25Score)
		}
	}
}

func TestFuse_OnlyBM25(t *testing.T) {
	assert := assertpkg.New(t)
	bm25 := []vector.Hit{{MessageID: 1, Rank: 1}, {MessageID: 2, Rank: 2}}
	out := Fuse(bm25, nil, 60, 1.0, nil, nil)
	requirepkg.Len(t, out, 2)
	assert.Equal(int64(1), out[0].MessageID)
	assert.Equal(int64(2), out[1].MessageID)
	for _, h := range out {
		assert.Truef(math.IsNaN(h.VectorScore),
			"msg %d VectorScore should be NaN for BM25-only, got %v", h.MessageID, h.VectorScore)
	}
}

func TestFuse_OnlyVector(t *testing.T) {
	vec := []vector.Hit{{MessageID: 10, Rank: 1}, {MessageID: 20, Rank: 2}}
	out := Fuse(nil, vec, 60, 1.0, nil, nil)
	requirepkg.Len(t, out, 2)
	assertpkg.Equal(t, int64(10), out[0].MessageID, "top")
	for _, h := range out {
		assertpkg.Truef(t, math.IsNaN(h.BM25Score),
			"msg %d BM25Score should be NaN for vector-only, got %v", h.MessageID, h.BM25Score)
	}
}

func TestFuse_Empty(t *testing.T) {
	out := Fuse(nil, nil, 60, 1.0, nil, nil)
	assertpkg.Empty(t, out)
}

func TestFuse_SubjectBoost(t *testing.T) {
	assert := assertpkg.New(t)
	// Both messages appear only in BM25 with identical rank sums after
	// boost differentiates them.
	bm25 := []vector.Hit{
		{MessageID: 1, Rank: 1}, // score 1/61
		{MessageID: 2, Rank: 1}, // score 1/61 — same rank, different list position
	}
	subjects := map[int64]string{
		1: "ordinary email",
		2: "Quarterly Review meeting",
	}
	terms := []string{"meeting"}
	out := Fuse(bm25, nil, 60, 2.0, terms, subjects)
	requirepkg.Len(t, out, 2)
	assert.Equalf(int64(2), out[0].MessageID, "top should be msg 2 (boosted); order: %+v", out)
	// The boosted hit carries the flag.
	for _, h := range out {
		if h.MessageID == 2 {
			assert.True(h.SubjectBoosted, "msg 2 should have SubjectBoosted=true")
		}
		if h.MessageID == 1 {
			assert.False(h.SubjectBoosted, "msg 1 should NOT be boosted")
		}
	}
}

func TestFuse_SubjectBoost_CaseInsensitive(t *testing.T) {
	bm25 := []vector.Hit{{MessageID: 1, Rank: 1}}
	subjects := map[int64]string{1: "MEETING Minutes"}
	out := Fuse(bm25, nil, 60, 2.0, []string{"meeting"}, subjects)
	requirepkg.Len(t, out, 1)
	assertpkg.Truef(t, out[0].SubjectBoosted, "case-insensitive match failed; out=%+v", out)
}

func TestFuse_NoBoostWhenFlagUnset(t *testing.T) {
	bm25 := []vector.Hit{{MessageID: 1, Rank: 1}}
	subjects := map[int64]string{1: "meeting subject"}
	out := Fuse(bm25, nil, 60, 1.0, []string{"meeting"}, subjects) // boost == 1.0
	assertpkg.False(t, out[0].SubjectBoosted, "SubjectBoosted should be false when boost <= 1.0")
}

// TestFuse_TiedRRFScoresStableByMessageID verifies that when two
// hits have identical RRF scores (e.g. swapped BM25/vector ranks
// that add to the same 1/(k+r1) + 1/(k+r2)), the returned order is
// deterministic — ascending by MessageID — rather than relying on
// Go map iteration, which would scramble ties across invocations.
func TestFuse_TiedRRFScoresStableByMessageID(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	// Two hits with BM25 rank 1 / vec rank 2 vs. BM25 rank 2 / vec
	// rank 1 have identical RRF scores (1/61 + 1/62).
	bm25 := []vector.Hit{{MessageID: 7, Rank: 1}, {MessageID: 3, Rank: 2}}
	vec := []vector.Hit{{MessageID: 3, Rank: 1}, {MessageID: 7, Rank: 2}}
	for i := range 20 {
		out := Fuse(bm25, vec, 60, 1.0, nil, nil)
		require.Lenf(out, 2, "iter %d", i)
		require.Equalf(out[0].RRFScore, out[1].RRFScore, "iter %d: scores differ, not a tie scenario: %+v", i, out)
		assert.Equalf(int64(3), out[0].MessageID, "iter %d: want 3 first (ascending MessageID on tie)", i)
		assert.Equalf(int64(7), out[1].MessageID, "iter %d: want 7 second", i)
	}
}

func TestFuse_ScorePreservedFromInputs(t *testing.T) {
	bm25 := []vector.Hit{{MessageID: 1, Rank: 1, Score: 5.5}}
	vec := []vector.Hit{{MessageID: 1, Rank: 1, Score: 0.9}}
	out := Fuse(bm25, vec, 60, 1.0, nil, nil)
	assertpkg.Equal(t, 5.5, out[0].BM25Score)
	assertpkg.InDelta(t, 0.9, out[0].VectorScore, 1e-6)
}
