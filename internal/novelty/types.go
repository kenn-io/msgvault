package novelty

import (
	"errors"
	"fmt"
)

const PolicyVersion = "novelty-v1"

type Lane string

const (
	LaneMandatory Lane = "mandatory"
	LaneRecency   Lane = "recency"
	LaneNovelty   Lane = "novelty"
	LaneFill      Lane = "fill"
)

type Mode string

const (
	ModeRecency         Mode = "recency"
	ModeNovelty         Mode = "novelty"
	ModeNoveltyInSample Mode = "novelty_in_sample"
	ModeRecencyFallback Mode = "recency_fallback"
)

type FallbackReason string

const (
	FallbackVectorDisabled     FallbackReason = "vector_disabled"
	FallbackNoActiveGeneration FallbackReason = "no_active_generation"
	FallbackIndexStale         FallbackReason = "index_stale"
	FallbackCapabilityMissing  FallbackReason = "capability_missing"
	FallbackLowCoverage        FallbackReason = "low_coverage"
	FallbackVectorLoadFailed   FallbackReason = "vector_load_failed"
	FallbackWorkLimit          FallbackReason = "work_limit"
)

var ErrInvalidInput = errors.New("novelty: invalid input")

type Params struct {
	Budget         int
	RecencyShareBP int
	// SupportWeightBP is how much a candidate's neighbor support scales its
	// novelty score: 0 ignores support and 10000 scores unsupported outliers 0.
	SupportWeightBP  int
	MinNoveltyBP     int
	MinCoverageBP    int
	SupportNeighbors int
	DuplicateCosBP   int
	MaxWorkUnits     int64
}

func DefaultParams(budget int) Params {
	return Params{
		Budget: budget, RecencyShareBP: 5000, SupportWeightBP: 5000,
		MinNoveltyBP: 200, MinCoverageBP: 5000, SupportNeighbors: 3,
		DuplicateCosBP: 9800, MaxWorkUnits: 1 << 28,
	}
}

func (p Params) Validate() error {
	if p.Budget < 1 || p.SupportNeighbors < 1 || p.SupportNeighbors > 64 || p.MaxWorkUnits < 1 {
		return fmt.Errorf("%w: budget, neighbors, or work limit out of range", ErrInvalidInput)
	}
	for _, v := range []int{p.RecencyShareBP, p.SupportWeightBP, p.MinNoveltyBP, p.MinCoverageBP} {
		if v < 0 || v > 10000 {
			return fmt.Errorf("%w: basis points out of range", ErrInvalidInput)
		}
	}
	if p.DuplicateCosBP < 1 || p.DuplicateCosBP > 10000 {
		return fmt.Errorf("%w: duplicate cosine out of range", ErrInvalidInput)
	}
	return nil
}

type Candidate struct {
	Key             string
	MessageID       int64
	RecencyRank     int
	Mandatory       bool
	OverlapOld      bool
	NoveltyEligible bool
}

type Input struct {
	Params     Params
	Candidates []Candidate
	// Vectors and Anchors must be successful MessageVector outputs: unit
	// message vectors of one shared dimension, rather than raw stored chunks.
	// Select falls back to recency when any of them is not.
	Vectors map[int64][]float32
	Anchors [][]float32
	// OverlapCap bounds non-mandatory old picks; -1 leaves overlap unlimited.
	OverlapCap int
}

type Pick struct {
	Key       string
	MessageID int64
	Lane      Lane
	NoveltyBP *int
	SupportBP *int
	ScoreBP   *int
}

type Result struct {
	Mode     Mode
	Fallback FallbackReason
	// Picks are in selection order: mandatory items first, then each lane in
	// turn. Callers that need recency order sort by candidate rank.
	Picks       []Pick
	ScoredCount int
}
