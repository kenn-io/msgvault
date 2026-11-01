package novelty

import (
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSelectInvariants(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // Deterministic synthetic fixtures, not cryptographic randomness.
	for caseID := range 1000 {
		t.Run(strconv.Itoa(caseID), func(t *testing.T) {
			assert := assert.New(t)
			count := r.IntN(41)
			in := Input{Params: DefaultParams(1 + r.IntN(12)), OverlapCap: r.IntN(5) - 1, Vectors: map[int64][]float32{}}
			clusters := r.IntN(3)
			coverage := 40 + r.IntN(61)
			randomVector := func() []float32 {
				v := make([]float32, 8)
				for i := range v {
					v[i] = float32(r.NormFloat64())
				}
				if clusters > 0 && r.IntN(2) == 0 {
					v = axis(r.IntN(clusters), 8)
					v[7] = float32(r.Float64() * .02)
				}
				out, ok := MessageVector([][]float32{v}, 8, 4)
				require.True(t, ok)
				return out
			}
			for i := range count {
				id := int64(i + 1)
				in.Candidates = append(in.Candidates, Candidate{Key: strconv.Itoa(i), MessageID: id, RecencyRank: i, OverlapOld: r.IntN(2) == 0, NoveltyEligible: r.IntN(2) == 0})
				if r.IntN(100) < coverage {
					in.Vectors[id] = randomVector()
				}
			}
			if count > 0 && r.IntN(2) == 0 {
				in.Candidates[r.IntN(count)].Mandatory = true
			}
			for range r.IntN(11) {
				in.Anchors = append(in.Anchors, randomVector())
			}
			got := selectOK(t, in)
			assert.LessOrEqual(len(got.Picks), in.Params.Budget)
			picked := map[string]bool{}
			old := 0
			// Check feasibility at each pick, including candidates skipped by recency.
			for _, p := range got.Picks {
				var c Candidate
				found := false
				for _, candidate := range in.Candidates {
					if candidate.Key == p.Key {
						c = candidate
						found = true
						break
					}
				}
				require.True(t, found)
				assert.False(picked[p.Key])
				assert.Equal(c.MessageID, p.MessageID)
				if !c.Mandatory {
					assert.True(propertyFeasible(c, in, got, picked, old), "selected candidate %s must be feasible", c.Key)
				}
				if !c.Mandatory && c.OverlapOld {
					old++
				}
				if p.Lane == LaneRecency {
					for _, earlier := range in.Candidates {
						if earlier.RecencyRank >= c.RecencyRank || picked[earlier.Key] {
							continue
						}
						assert.False(propertyFeasible(earlier, in, got, picked, old-boolInt(c.OverlapOld && !c.Mandatory)))
					}
				}
				if p.Lane == LaneNovelty {
					assert.True(c.NoveltyEligible)
					assert.Contains(in.Vectors, c.MessageID)
					assert.NotNil(p.ScoreBP)
				} else {
					assert.Nil(p.ScoreBP)
				}
				if _, ok := in.Vectors[c.MessageID]; !ok {
					assert.Contains([]Lane{LaneMandatory, LaneRecency, LaneFill}, p.Lane)
					assert.Nil(p.NoveltyBP)
					assert.Nil(p.SupportBP)
				}
				picked[p.Key] = true
			}
			for _, c := range in.Candidates {
				if c.Mandatory {
					assert.True(picked[c.Key])
				}
				if len(got.Picks) < in.Params.Budget && !picked[c.Key] {
					assert.False(propertyFeasible(c, in, got, picked, old))
				}
			}
			if in.OverlapCap >= 0 {
				assert.LessOrEqual(old, in.OverlapCap)
			}
			for range 2 {
				shuffled := in
				shuffled.Candidates = slices.Clone(in.Candidates)
				shuffled.Anchors = slices.Clone(in.Anchors)
				r.Shuffle(len(shuffled.Candidates), func(i, j int) {
					shuffled.Candidates[i], shuffled.Candidates[j] = shuffled.Candidates[j], shuffled.Candidates[i]
				})
				r.Shuffle(len(shuffled.Anchors), func(i, j int) { shuffled.Anchors[i], shuffled.Anchors[j] = shuffled.Anchors[j], shuffled.Anchors[i] })
				assert.Equal(got, selectOK(t, shuffled))
			}
		})
	}
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func propertyFeasible(c Candidate, in Input, got Result, picked map[string]bool, old int) bool {
	if c.OverlapOld && !c.Mandatory && in.OverlapCap >= 0 && old >= in.OverlapCap {
		return false
	}
	if got.Mode == ModeRecencyFallback {
		return true
	}
	v, ok := in.Vectors[c.MessageID]
	if !ok {
		return true
	}
	for _, p := range got.Picks {
		if !picked[p.Key] {
			continue
		}
		if other, ok := in.Vectors[p.MessageID]; ok && micro(dot(v, other)) >= int64(in.Params.DuplicateCosBP)*100 {
			return false
		}
	}
	return true
}
