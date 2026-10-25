package novelty

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func axis(i, dim int) []float32 { v := make([]float32, dim); v[i] = 1; return v }
func theme(t *testing.T, a, b int) []float32 {
	t.Helper()
	v := axis(a, 8)
	v[b] = .5
	out, ok := MessageVector([][]float32{v}, 8, 4)
	require.True(t, ok)
	return out
}
func selectionInput(vectors ...[]float32) Input {
	in := Input{Params: DefaultParams(3), Vectors: map[int64][]float32{}, OverlapCap: -1}
	for i, v := range vectors {
		id := int64(i + 1)
		in.Candidates = append(in.Candidates, Candidate{Key: strconv.Itoa(i), MessageID: id, RecencyRank: i, NoveltyEligible: true})
		if v != nil {
			in.Vectors[id] = v
		}
	}
	return in
}
func keys(r Result) []string {
	out := make([]string, 0, len(r.Picks))
	for _, p := range r.Picks {
		out = append(out, p.Key)
	}
	return out
}
func selectOK(t *testing.T, in Input) Result {
	t.Helper()
	r, err := Select(context.Background(), in)
	require.NoError(t, err)
	return r
}
func themeInput(t *testing.T) Input {
	t.Helper()
	in := selectionInput(theme(t, 0, 4), theme(t, 0, 5), theme(t, 0, 6), theme(t, 1, 2), theme(t, 0, 7), theme(t, 1, 3))
	in.Anchors = [][]float32{axis(0, 8)}
	return in
}
func TestSelectNoveltySurfacesUnknownTheme(t *testing.T) {
	assert := assert.New(t)
	in := themeInput(t)
	got := selectOK(t, in)
	assert.Equal([]string{"0", "1", "3"}, keys(got))
	assert.Equal([]Lane{LaneRecency, LaneRecency, LaneNovelty}, []Lane{got.Picks[0].Lane, got.Picks[1].Lane, got.Picks[2].Lane})
	assert.Equal(10000, *got.Picks[2].NoveltyBP)
	r, err := Recency(in)
	require.NoError(t, err)
	assert.Equal([]string{"0", "1", "2"}, keys(r))
}
func TestSelectSupportWeight(t *testing.T) {
	// Every candidate is equally far from the anchor; only "1" and "2" support
	// each other, and "0" is the newest.
	for _, tc := range []struct {
		weight int
		want   string
	}{{0, "0"}, {5000, "1"}, {10000, "1"}} {
		t.Run(strconv.Itoa(tc.weight), func(t *testing.T) {
			in := selectionInput(axis(3, 8), theme(t, 1, 4), theme(t, 1, 5))
			in.Anchors = [][]float32{axis(0, 8)}
			in.Params = DefaultParams(1)
			in.Params.RecencyShareBP = 0
			in.Params.SupportWeightBP = tc.weight
			assert.Equal(t, []string{tc.want}, keys(selectOK(t, in)))
		})
	}
}
func TestSelectNothingNewEqualsRecencySet(t *testing.T) {
	in := selectionInput(theme(t, 0, 4), theme(t, 0, 5), theme(t, 0, 6), theme(t, 0, 7))
	for _, v := range in.Vectors {
		in.Anchors = append(in.Anchors, v)
	}
	got := selectOK(t, in)
	r, err := Recency(in)
	require.NoError(t, err)
	assert.Equal(t, keys(r), keys(got))
	for _, p := range got.Picks {
		assert.NotEqual(t, LaneNovelty, p.Lane)
	}
}
func TestSelectInSampleOnePerTheme(t *testing.T) {
	in := selectionInput(theme(t, 0, 3), theme(t, 0, 4), theme(t, 0, 5), theme(t, 1, 6), theme(t, 1, 7))
	in.Params = DefaultParams(2)
	in.Params.RecencyShareBP = 0
	in.Params.SupportNeighbors = 2
	got := selectOK(t, in)
	assert.Equal(t, ModeNoveltyInSample, got.Mode)
	assert.Equal(t, []string{"3", "0"}, keys(got))
}
func tenAxes() Input {
	vs := make([][]float32, 10)
	for i := range vs {
		vs[i] = axis(i, 10)
	}
	in := selectionInput(vs...)
	in.Params = DefaultParams(8)
	return in
}
func TestSelectAllOld(t *testing.T) {
	in := tenAxes()
	in.OverlapCap = 2
	for i := range in.Candidates {
		in.Candidates[i].OverlapOld = true
	}
	assert.Len(t, selectOK(t, in).Picks, 2)
	r, err := Recency(in)
	require.NoError(t, err)
	assert.Len(t, r.Picks, 2)
}
func TestSelectAllDuplicates(t *testing.T) {
	in := tenAxes()
	for id := range in.Vectors {
		in.Vectors[id] = axis(0, 10)
	}
	assert.Len(t, selectOK(t, in).Picks, 1)
	r, err := Recency(in)
	require.NoError(t, err)
	assert.Len(t, r.Picks, 8)
}
func TestSelectMandatoryWithOverlap(t *testing.T) {
	in := tenAxes()
	in.Params.Budget = 7
	in.OverlapCap = 1
	in.Candidates[9].Mandatory = true
	for i := 4; i < 10; i++ {
		in.Candidates[i].OverlapOld = true
	}
	assert.ElementsMatch(t, []string{"9", "0", "1", "2", "3", "4"}, keys(selectOK(t, in)))
	r, err := Recency(in)
	require.NoError(t, err)
	assert.Equal(t, []string{"9", "0", "1", "2", "3", "4"}, keys(r))
}
func TestSelectSameMessageCandidatesAreDuplicates(t *testing.T) {
	in := selectionInput(axis(0, 8), axis(0, 8))
	in.Candidates[1].MessageID = 1
	assert.Len(t, selectOK(t, in).Picks, 1)
	r, err := Recency(in)
	require.NoError(t, err)
	assert.Len(t, r.Picks, 2)
}
func assertFallback(t *testing.T, in Input, reason FallbackReason) {
	t.Helper()
	got := selectOK(t, in)
	r, err := Recency(in)
	require.NoError(t, err)
	assert.Equal(t, ModeRecencyFallback, got.Mode)
	assert.Equal(t, reason, got.Fallback)
	assert.Equal(t, keys(r), keys(got))
}
func TestSelectLowCoverageFallsBack(t *testing.T) {
	in := tenAxes()
	for id := int64(5); id <= 10; id++ {
		delete(in.Vectors, id)
	}
	assertFallback(t, in, FallbackLowCoverage)
	assert.Equal(t, 4, selectOK(t, in).ScoredCount)
}
func TestSelectNoVectorsFallsBackWithoutCoverageFloor(t *testing.T) {
	in := selectionInput(nil, nil)
	in.Params.MinCoverageBP = 0
	assertFallback(t, in, FallbackLowCoverage)
}
func TestSelectWorkLimitFallsBack(t *testing.T) {
	in := tenAxes()
	in.Params.MaxWorkUnits = 1
	assertFallback(t, in, FallbackWorkLimit)
}
func TestSelectDimensionMismatchFallsBack(t *testing.T) {
	in := tenAxes()
	in.Vectors[1] = axis(0, 4)
	assertFallback(t, in, FallbackVectorLoadFailed)
}
func TestSelectInvalidVectorFallsBack(t *testing.T) {
	for _, v := range []float32{float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1)), 1e20, .5} {
		t.Run(fmt.Sprint(v), func(t *testing.T) {
			in := tenAxes()
			in.Vectors[1][0] = v
			assertFallback(t, in, FallbackVectorLoadFailed)
			in = tenAxes()
			in.Anchors = [][]float32{axis(0, 10)}
			in.Anchors[0][0] = v
			assertFallback(t, in, FallbackVectorLoadFailed)
		})
	}
}
func TestSelectUnscoredStayInRecencyAndFill(t *testing.T) {
	assert := assert.New(t)
	in := tenAxes()
	for id := int64(7); id <= 10; id++ {
		delete(in.Vectors, id)
	}
	for _, p := range selectOK(t, in).Picks {
		if _, ok := in.Vectors[p.MessageID]; !ok {
			assert.Contains([]Lane{LaneRecency, LaneFill}, p.Lane)
			assert.Nil(p.NoveltyBP)
			assert.Nil(p.SupportBP)
			assert.Nil(p.ScoreBP)
		}
	}
}
func TestSelectIneligibleNeverInNoveltyLane(t *testing.T) {
	in := themeInput(t)
	in.Candidates[3].NoveltyEligible = false
	for _, p := range selectOK(t, in).Picks {
		if p.Key == "3" {
			assert.NotEqual(t, LaneNovelty, p.Lane)
		}
	}
}
func TestSelectHonorsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Select(ctx, tenAxes())
	assert.ErrorIs(t, err, context.Canceled)
}
func TestSelectRejectsInvalidInput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Input)
	}{
		{"params", func(in *Input) { in.Params.Budget = 0 }}, {"empty key", func(in *Input) { in.Candidates[0].Key = "" }}, {"duplicate key", func(in *Input) { in.Candidates[0].Key = in.Candidates[1].Key }}, {"message id", func(in *Input) { in.Candidates[0].MessageID = 0 }}, {"rank", func(in *Input) { in.Candidates[0].RecencyRank = -1 }}, {"duplicate rank", func(in *Input) { in.Candidates[0].RecencyRank = 1 }}, {"mandatory", func(in *Input) {
			in.Params.Budget = 1
			in.Candidates[0].Mandatory = true
			in.Candidates[1].Mandatory = true
		}}, {"overlap", func(in *Input) { in.OverlapCap = -2 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := tenAxes()
			tc.change(&in)
			t.Run("select", func(t *testing.T) {
				_, err := Select(context.Background(), in)
				assert.ErrorIs(t, err, ErrInvalidInput)
			})
			t.Run("recency", func(t *testing.T) {
				_, err := Recency(in)
				assert.ErrorIs(t, err, ErrInvalidInput)
			})
		})
	}
	for _, reason := range []FallbackReason{"", "unknown"} {
		t.Run("fallback/"+string(reason), func(t *testing.T) {
			_, err := Fallback(tenAxes(), reason)
			assert.ErrorIs(t, err, ErrInvalidInput)
		})
	}
}

func goldenInput(t *testing.T) Input {
	t.Helper()
	r := rand.New(rand.NewPCG(2026, 1022)) //nolint:gosec // Deterministic synthetic fixtures, not cryptographic randomness.
	centers := make([][]float32, 5)
	for i := range centers {
		centers[i] = make([]float32, 32)
		for j := range centers[i] {
			centers[i][j] = float32(r.NormFloat64())
		}
	}
	vector := func(i int) []float32 {
		v := make([]float32, 32)
		for j := range v {
			v[j] = centers[i%5][j] + float32(.25*r.NormFloat64())
		}
		v, ok := MessageVector([][]float32{v}, 32, 4)
		require.True(t, ok)
		return v
	}
	in := Input{Params: DefaultParams(8), OverlapCap: 2, Vectors: map[int64][]float32{}}
	for i := range 40 {
		id := int64(i + 1)
		in.Candidates = append(in.Candidates, Candidate{Key: fmt.Sprintf("candidate-%02d", i), MessageID: id, RecencyRank: i, OverlapOld: i < 5, Mandatory: i == 39, NoveltyEligible: true})
		if i < 30 {
			in.Vectors[id] = vector(i)
		}
	}
	for i := range 12 {
		in.Anchors = append(in.Anchors, vector(i))
	}
	return in
}
func TestSelectGolden(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	in := goldenInput(t)
	got := selectOK(t, in)
	intp := func(v int) *int { return &v }
	want := []Pick{
		{Key: "candidate-39", MessageID: 40, Lane: LaneMandatory},
		{Key: "candidate-00", MessageID: 1, Lane: LaneRecency, NoveltyBP: intp(458), SupportBP: intp(3103)},
		{Key: "candidate-01", MessageID: 2, Lane: LaneRecency, NoveltyBP: intp(436), SupportBP: intp(344)},
		{Key: "candidate-05", MessageID: 6, Lane: LaneRecency, NoveltyBP: intp(543), SupportBP: intp(4827)},
		{Key: "candidate-06", MessageID: 7, Lane: LaneRecency, NoveltyBP: intp(679), SupportBP: intp(1379)},
		{Key: "candidate-29", MessageID: 30, Lane: LaneNovelty, NoveltyBP: intp(587), SupportBP: intp(6551), ScoreBP: intp(485)},
		{Key: "candidate-27", MessageID: 28, Lane: LaneNovelty, NoveltyBP: intp(526), SupportBP: intp(7241), ScoreBP: intp(453)},
		{Key: "candidate-22", MessageID: 23, Lane: LaneNovelty, NoveltyBP: intp(590), SupportBP: intp(5172), ScoreBP: intp(447)},
	}
	assert.Equal(want, got.Picks)
	fp, err := in.Params.Fingerprint()
	require.NoError(err)
	d := DigestInput{ParamsFingerprint: fp, Mode: got.Mode, Picks: got.Picks}
	for _, c := range in.Candidates {
		d.CandidateKeys = append(d.CandidateKeys, c.Key)
	}
	sha, err := SelectionSHA256(d)
	require.NoError(err)
	assert.Equal("c4c249f3aa538bf2e693c8b82762f88c37b9d4fa5f2961d9d9a6a67b077f16c1", sha)
}
