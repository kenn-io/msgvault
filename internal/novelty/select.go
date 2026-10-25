package novelty

import (
	"context"
	"fmt"
	"slices"
)

func orderedInput(in Input) ([]Candidate, int, error) {
	if err := in.Params.Validate(); err != nil {
		return nil, 0, err
	}
	if in.OverlapCap < -1 {
		return nil, 0, fmt.Errorf("%w: overlap cap below -1", ErrInvalidInput)
	}
	keys, ranks := map[string]bool{}, map[int]bool{}
	mandatory, scored := 0, 0
	for _, c := range in.Candidates {
		if c.Key == "" || keys[c.Key] || c.MessageID <= 0 || c.RecencyRank < 0 || ranks[c.RecencyRank] {
			return nil, 0, fmt.Errorf("%w: invalid candidate coordinates", ErrInvalidInput)
		}
		keys[c.Key] = true
		ranks[c.RecencyRank] = true
		if c.Mandatory {
			mandatory++
		}
		if _, ok := in.Vectors[c.MessageID]; ok {
			scored++
		}
	}
	if mandatory > in.Params.Budget {
		return nil, 0, fmt.Errorf("%w: mandatory count exceeds budget", ErrInvalidInput)
	}
	ordered := slices.Clone(in.Candidates)
	slices.SortFunc(ordered, func(a, b Candidate) int {
		if a.RecencyRank < b.RecencyRank {
			return -1
		}
		if a.RecencyRank > b.RecencyRank {
			return 1
		}
		return 0
	})
	return ordered, scored, nil
}

// Recency keeps mandatory items first, then the caller's existing order.
func Recency(in Input) (Result, error) {
	ordered, scored, err := orderedInput(in)
	if err != nil {
		return Result{}, err
	}
	result := Result{Mode: ModeRecency, ScoredCount: scored, Picks: make([]Pick, 0, min(in.Params.Budget, len(ordered)))}
	for _, c := range ordered {
		if c.Mandatory {
			result.Picks = append(result.Picks, Pick{Key: c.Key, MessageID: c.MessageID, Lane: LaneMandatory})
		}
	}
	overlap := 0
	for _, c := range ordered {
		if len(result.Picks) == in.Params.Budget {
			break
		}
		if c.Mandatory || (c.OverlapOld && in.OverlapCap >= 0 && overlap >= in.OverlapCap) {
			continue
		}
		result.Picks = append(result.Picks, Pick{Key: c.Key, MessageID: c.MessageID, Lane: LaneRecency})
		if c.OverlapOld {
			overlap++
		}
	}
	return result, nil
}

// Fallback applies recency selection and records why novelty was unavailable.
func Fallback(in Input, reason FallbackReason) (Result, error) {
	switch reason {
	case FallbackVectorDisabled, FallbackNoActiveGeneration, FallbackIndexStale, FallbackCapabilityMissing, FallbackLowCoverage, FallbackVectorLoadFailed, FallbackWorkLimit:
	default:
		return Result{}, fmt.Errorf("%w: unknown fallback reason %q", ErrInvalidInput, reason)
	}
	result, err := Recency(in)
	if err != nil {
		return Result{}, err
	}
	result.Mode = ModeRecencyFallback
	result.Fallback = reason
	return result, nil
}

type selector struct {
	in                Input
	ordered           []Candidate
	result            Result
	indices           []int
	gram              [][]int64
	base, cur, factor []int64
	support           []int
	picked, blocked   []bool
	overlap           int
	scoredPicked      bool
}

// Select fills mandatory, recency, novelty, and fill lanes using exact cosine
// scores. Vector failures preserve the caller's recency behavior.
//
// After any scored pick, candidates within DuplicateCosBP of it are dropped
// from every later lane, so the recency lane keeps the newest distinct
// messages and Select can return fewer picks than Recency.
func Select(ctx context.Context, in Input) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	ordered, scored, err := orderedInput(in)
	if err != nil {
		return Result{}, err
	}
	dimension := 0
	for _, c := range ordered {
		if v, ok := in.Vectors[c.MessageID]; ok {
			if dimension == 0 {
				dimension = len(v)
			}
			if !unitVector(v, dimension) {
				return Fallback(in, FallbackVectorLoadFailed)
			}
		}
	}
	for _, v := range in.Anchors {
		if dimension == 0 {
			dimension = len(v)
		}
		if !unitVector(v, dimension) {
			return Fallback(in, FallbackVectorLoadFailed)
		}
	}
	if scored == 0 || int64(scored)*10000/int64(len(ordered)) < int64(in.Params.MinCoverageBP) {
		return Fallback(in, FallbackLowCoverage)
	}
	// Divide the bound to avoid overflowing the work product.
	if scored > 0 && (int64(scored) > in.Params.MaxWorkUnits/int64(max(1, dimension))/int64(scored+len(in.Anchors))) {
		return Fallback(in, FallbackWorkLimit)
	}
	state := selector{
		in: in, ordered: ordered,
		result: Result{
			Mode: ModeNovelty, ScoredCount: scored,
			Picks: make([]Pick, 0, min(in.Params.Budget, len(ordered))),
		},
		indices: make([]int, len(ordered)),
		picked:  make([]bool, len(ordered)), blocked: make([]bool, len(ordered)),
	}
	if len(in.Anchors) == 0 {
		state.result.Mode = ModeNoveltyInSample
	}
	vectors := make([][]float32, 0, scored)
	for i, c := range ordered {
		state.indices[i] = -1
		if v, ok := in.Vectors[c.MessageID]; ok {
			state.indices[i] = len(vectors)
			vectors = append(vectors, v)
		}
	}
	if err := state.prepare(ctx, vectors); err != nil {
		return Result{}, err
	}
	for i, c := range ordered {
		if c.Mandatory {
			state.pick(i, LaneMandatory, 0)
		}
	}
	// ceil(Budget * share / 10000), without overflowing int.
	quota := in.Params.Budget/10000*in.Params.RecencyShareBP + (in.Params.Budget%10000*in.Params.RecencyShareBP+9999)/10000
	quota = min(quota, in.Params.Budget-len(state.result.Picks))
	for i := range ordered {
		if quota == 0 {
			break
		}
		if state.feasible(i) {
			state.pick(i, LaneRecency, 0)
			quota--
		}
	}
	for len(state.result.Picks) < in.Params.Budget {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		best := -1
		var score int64
		for i, c := range ordered {
			j := state.indices[i]
			if j < 0 || !c.NoveltyEligible || !state.feasible(i) || state.cur[j] < int64(in.Params.MinNoveltyBP)*100 {
				continue
			}
			value := state.cur[j] * state.factor[j] / 10000
			if best < 0 || value > score {
				best = i
				score = value
			}
		}
		if best < 0 {
			break
		}
		state.pick(best, LaneNovelty, score)
	}
	for i := range ordered {
		if len(state.result.Picks) == in.Params.Budget {
			break
		}
		if state.feasible(i) {
			state.pick(i, LaneFill, 0)
		}
	}
	return state.result, nil
}

func (s *selector) prepare(ctx context.Context, vectors [][]float32) error {
	n := len(vectors)
	s.gram = make([][]int64, n)
	s.base = make([]int64, n)
	s.cur = make([]int64, n)
	s.factor = make([]int64, n)
	s.support = make([]int, n)
	neighbors := make([]int64, n)
	for i, v := range vectors {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.gram[i] = make([]int64, n)
		values := make([]int64, 0, n)
		for j, w := range vectors {
			value := micro(dot(v, w))
			s.gram[i][j] = value
			if i != j && value < int64(s.in.Params.DuplicateCosBP)*100 {
				values = append(values, value)
			}
		}
		slices.Sort(values)
		neighbors[i] = -microOne
		if len(values) > 0 {
			top := values[max(0, len(values)-s.in.Params.SupportNeighbors):]
			var sum int64
			for _, value := range top {
				sum += value
			}
			neighbors[i] = sum / int64(len(top))
		}
		if len(s.in.Anchors) > 0 {
			nearest := int64(-microOne)
			for _, a := range s.in.Anchors {
				nearest = max(nearest, micro(dot(v, a)))
			}
			s.base[i] = clampMicro(microOne - nearest)
		} else {
			s.base[i] = clampMicro(microOne - neighbors[i])
		}
		s.cur[i] = s.base[i]
	}
	for i, neighbor := range neighbors {
		rank := 0
		for _, other := range neighbors {
			if other < neighbor {
				rank++
			}
		}
		if n > 1 {
			s.support[i] = rank * 10000 / (n - 1)
		}
		// A novelty score is cur * factor / 10000, where factor scales from
		// 10000 - SupportWeightBP for the least-supported candidate to 10000.
		weight := int64(s.in.Params.SupportWeightBP)
		s.factor[i] = 10000 - weight + weight*int64(s.support[i])/10000
	}
	return nil
}

func (s *selector) feasible(i int) bool {
	c := s.ordered[i]
	return !s.picked[i] && !s.blocked[i] && (!c.OverlapOld || c.Mandatory || s.in.OverlapCap < 0 || s.overlap < s.in.OverlapCap)
}

func (s *selector) pick(i int, lane Lane, score int64) {
	c := s.ordered[i]
	p := Pick{Key: c.Key, MessageID: c.MessageID, Lane: lane}
	j := s.indices[i]
	if j >= 0 {
		novelty, support := int(s.base[j]/100), s.support[j]
		p.NoveltyBP = &novelty
		p.SupportBP = &support
		if lane == LaneNovelty {
			value := int(score / 100)
			p.ScoreBP = &value
		}
		for k, index := range s.indices {
			if index < 0 {
				continue
			}
			distance := clampMicro(microOne - s.gram[index][j])
			if !s.scoredPicked && len(s.in.Anchors) == 0 {
				s.cur[index] = distance
			} else {
				s.cur[index] = min(s.cur[index], distance)
			}
			s.blocked[k] = s.blocked[k] || s.gram[index][j] >= int64(s.in.Params.DuplicateCosBP)*100
		}
		s.scoredPicked = true
	}
	s.picked[i] = true
	if c.OverlapOld && !c.Mandatory {
		s.overlap++
	}
	s.result.Picks = append(s.result.Picks, p)
}
