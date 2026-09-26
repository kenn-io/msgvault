// Package rerank scores and reorders search candidates with TypeSafe Jev.
package rerank

import (
	"fmt"
	"math"
	"slices"
)

// Usage records attempted requests and provider token accounting. Complete is
// false when at least one attempt lacks usage, so token counts are subtotals.
type Usage struct {
	Requests     int
	InputTokens  *int64
	OutputTokens *int64
	Complete     bool
}

// Result contains one score for every candidate, in the request's order.
type Result struct {
	Scores []float64
	Usage  Usage
}

// Request is one scoring operation over a fixed candidate list.
type Request struct {
	Query      string
	Candidates []string
}

// Order returns stable descending score indices. It rejects a score that
// cannot represent a probability and leaves scores untouched.
func Order(scores []float64) ([]int, error) {
	for i, score := range scores {
		if math.IsNaN(score) || math.IsInf(score, 0) || score < 0 || score > 1 {
			return nil, fmt.Errorf("score %d is not a finite probability in [0,1]", i)
		}
	}
	indices := make([]int, len(scores))
	for i := range indices {
		indices[i] = i
	}
	slices.SortStableFunc(indices, func(i, j int) int {
		switch {
		case scores[i] > scores[j]:
			return -1
		case scores[i] < scores[j]:
			return 1
		default:
			return 0
		}
	})
	return indices, nil
}
