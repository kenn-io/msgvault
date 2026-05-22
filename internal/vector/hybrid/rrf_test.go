package hybrid

import (
	"math"
	"testing"

	"go.kenn.io/msgvault/internal/vector"
)

func TestFuse_BothSignalsContribute(t *testing.T) {
	bm25 := []vector.Hit{{MessageID: 1, Rank: 1}, {MessageID: 2, Rank: 2}, {MessageID: 3, Rank: 3}}
	vec := []vector.Hit{{MessageID: 2, Rank: 1}, {MessageID: 4, Rank: 2}, {MessageID: 1, Rank: 3}}
	out := Fuse(bm25, vec, 60, 1.0, nil, nil)
	if len(out) != 4 {
		t.Fatalf("got %d, want 4", len(out))
	}
	// Msg 2: BM25 rank 2 (1/62) + vec rank 1 (1/61) ≈ 0.03251 → highest.
	// Msg 1: BM25 rank 1 (1/61) + vec rank 3 (1/63) ≈ 0.03226.
	if out[0].MessageID != 2 {
		t.Errorf("top = %d, want 2", out[0].MessageID)
	}
	// Msg 3 is only in BM25; msg 4 only in vec. Both should have one NaN.
	for _, h := range out {
		switch h.MessageID {
		case 3:
			if math.IsNaN(h.BM25Score) {
				t.Errorf("msg 3 BM25 should be non-NaN (or zero; Score=0)")
			}
			if !math.IsNaN(h.VectorScore) {
				t.Errorf("msg 3 VectorScore should be NaN, got %v", h.VectorScore)
			}
		case 4:
			if !math.IsNaN(h.BM25Score) {
				t.Errorf("msg 4 BM25Score should be NaN, got %v", h.BM25Score)
			}
		}
	}
}

func TestFuse_OnlyBM25(t *testing.T) {
	bm25 := []vector.Hit{{MessageID: 1, Rank: 1}, {MessageID: 2, Rank: 2}}
	out := Fuse(bm25, nil, 60, 1.0, nil, nil)
	if len(out) != 2 {
		t.Fatalf("got %d, want 2", len(out))
	}
	if out[0].MessageID != 1 || out[1].MessageID != 2 {
		t.Errorf("order = [%d, %d], want [1, 2]", out[0].MessageID, out[1].MessageID)
	}
	for _, h := range out {
		if !math.IsNaN(h.VectorScore) {
			t.Errorf("msg %d VectorScore should be NaN for BM25-only, got %v", h.MessageID, h.VectorScore)
		}
	}
}

func TestFuse_OnlyVector(t *testing.T) {
	vec := []vector.Hit{{MessageID: 10, Rank: 1}, {MessageID: 20, Rank: 2}}
	out := Fuse(nil, vec, 60, 1.0, nil, nil)
	if len(out) != 2 {
		t.Fatalf("got %d, want 2", len(out))
	}
	if out[0].MessageID != 10 {
		t.Errorf("top = %d, want 10", out[0].MessageID)
	}
	for _, h := range out {
		if !math.IsNaN(h.BM25Score) {
			t.Errorf("msg %d BM25Score should be NaN for vector-only, got %v", h.MessageID, h.BM25Score)
		}
	}
}

func TestFuse_Empty(t *testing.T) {
	out := Fuse(nil, nil, 60, 1.0, nil, nil)
	if len(out) != 0 {
		t.Errorf("got %d, want 0", len(out))
	}
}

func TestFuse_SubjectBoost(t *testing.T) {
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
	if len(out) != 2 {
		t.Fatalf("got %d, want 2", len(out))
	}
	if out[0].MessageID != 2 {
		t.Errorf("top = %d, want 2 (boosted); order: %+v", out[0].MessageID, out)
	}
	// The boosted hit carries the flag.
	for _, h := range out {
		if h.MessageID == 2 && !h.SubjectBoosted {
			t.Error("msg 2 should have SubjectBoosted=true")
		}
		if h.MessageID == 1 && h.SubjectBoosted {
			t.Error("msg 1 should NOT be boosted")
		}
	}
}

func TestFuse_SubjectBoost_CaseInsensitive(t *testing.T) {
	bm25 := []vector.Hit{{MessageID: 1, Rank: 1}}
	subjects := map[int64]string{1: "MEETING Minutes"}
	out := Fuse(bm25, nil, 60, 2.0, []string{"meeting"}, subjects)
	if len(out) != 1 || !out[0].SubjectBoosted {
		t.Errorf("case-insensitive match failed; out=%+v", out)
	}
}

func TestFuse_NoBoostWhenFlagUnset(t *testing.T) {
	bm25 := []vector.Hit{{MessageID: 1, Rank: 1}}
	subjects := map[int64]string{1: "meeting subject"}
	out := Fuse(bm25, nil, 60, 1.0, []string{"meeting"}, subjects) // boost == 1.0
	if out[0].SubjectBoosted {
		t.Error("SubjectBoosted should be false when boost <= 1.0")
	}
}

// TestFuse_TiedRRFScoresStableByMessageID verifies that when two
// hits have identical RRF scores (e.g. swapped BM25/vector ranks
// that add to the same 1/(k+r1) + 1/(k+r2)), the returned order is
// deterministic — ascending by MessageID — rather than relying on
// Go map iteration, which would scramble ties across invocations.
func TestFuse_TiedRRFScoresStableByMessageID(t *testing.T) {
	// Two hits with BM25 rank 1 / vec rank 2 vs. BM25 rank 2 / vec
	// rank 1 have identical RRF scores (1/61 + 1/62).
	bm25 := []vector.Hit{{MessageID: 7, Rank: 1}, {MessageID: 3, Rank: 2}}
	vec := []vector.Hit{{MessageID: 3, Rank: 1}, {MessageID: 7, Rank: 2}}
	for i := 0; i < 20; i++ {
		out := Fuse(bm25, vec, 60, 1.0, nil, nil)
		if len(out) != 2 {
			t.Fatalf("iter %d: got %d, want 2", i, len(out))
		}
		if out[0].RRFScore != out[1].RRFScore {
			t.Fatalf("iter %d: scores differ, not a tie scenario: %+v", i, out)
		}
		if out[0].MessageID != 3 || out[1].MessageID != 7 {
			t.Errorf("iter %d: order=[%d,%d], want [3,7] (ascending MessageID on tie)",
				i, out[0].MessageID, out[1].MessageID)
		}
	}
}

func TestFuse_ScorePreservedFromInputs(t *testing.T) {
	bm25 := []vector.Hit{{MessageID: 1, Rank: 1, Score: 5.5}}
	vec := []vector.Hit{{MessageID: 1, Rank: 1, Score: 0.9}}
	out := Fuse(bm25, vec, 60, 1.0, nil, nil)
	if out[0].BM25Score != 5.5 {
		t.Errorf("BM25Score=%v, want 5.5", out[0].BM25Score)
	}
	if out[0].VectorScore != 0.9 {
		t.Errorf("VectorScore=%v, want 0.9", out[0].VectorScore)
	}
}
