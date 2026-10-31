package novelty

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParamsValidate(t *testing.T) {
	require.NoError(t, DefaultParams(1).Validate())
	cases := []struct {
		name   string
		change func(*Params)
	}{
		{"budget", func(p *Params) { p.Budget = 0 }},
		{"recency low", func(p *Params) { p.RecencyShareBP = -1 }}, {"recency high", func(p *Params) { p.RecencyShareBP = 10001 }},
		{"support low", func(p *Params) { p.SupportWeightBP = -1 }}, {"support high", func(p *Params) { p.SupportWeightBP = 10001 }},
		{"novelty low", func(p *Params) { p.MinNoveltyBP = -1 }}, {"novelty high", func(p *Params) { p.MinNoveltyBP = 10001 }},
		{"coverage low", func(p *Params) { p.MinCoverageBP = -1 }}, {"coverage high", func(p *Params) { p.MinCoverageBP = 10001 }},
		{"duplicate low", func(p *Params) { p.DuplicateCosBP = 0 }}, {"duplicate high", func(p *Params) { p.DuplicateCosBP = 10001 }},
		{"neighbors low", func(p *Params) { p.SupportNeighbors = 0 }}, {"neighbors high", func(p *Params) { p.SupportNeighbors = 65 }},
		{"work", func(p *Params) { p.MaxWorkUnits = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := DefaultParams(2)
			tc.change(&p)
			assert.ErrorIs(t, p.Validate(), ErrInvalidInput)
		})
	}
}

func TestParamsFingerprintGolden(t *testing.T) {
	got, err := DefaultParams(3).Fingerprint()
	require.NoError(t, err)
	assert.Equal(t, "12185952ea6912f352e38fcb1d76fc3ae9fb19cbf5ae8bc64fe255eeeb0b9922", got)
}

func TestSelectionSHA256Golden(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := DigestInput{ParamsFingerprint: "params", Mode: ModeNovelty, VectorFingerprint: "vectors", AnchorSetSHA256: "anchors", CandidateKeys: []string{"b", "a"}, Picks: []Pick{{Key: "a", Lane: LaneRecency}, {Key: "b", Lane: LaneNovelty}}}
	got, err := SelectionSHA256(d)
	require.NoError(err)
	assert.Equal("710720e34380bc99cc3501e1a4b8d28ad0c80a50e31186cd855b69c02cf8a2f1", got)
	slices.Reverse(d.CandidateKeys)
	reordered, err := SelectionSHA256(d)
	require.NoError(err)
	assert.Equal(got, reordered)
	slices.Reverse(d.Picks)
	reordered, err = SelectionSHA256(d)
	require.NoError(err)
	assert.NotEqual(got, reordered)
	slices.Reverse(d.Picks)
	d.Picks[0].Lane = LaneFill
	reordered, err = SelectionSHA256(d)
	require.NoError(err)
	assert.NotEqual(got, reordered)
}

func TestAnchorSetSHA256SortsAndDedupes(t *testing.T) {
	assert.Equal(t, AnchorSetSHA256([]int64{1, 3}), AnchorSetSHA256([]int64{3, 1, 3}))
	assert.NotEmpty(t, AnchorSetSHA256([]int64{1, 3}))
	assert.Empty(t, AnchorSetSHA256(nil))
}
