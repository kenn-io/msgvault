package document

import (
	"slices"
	"sort"

	"go.kenn.io/msgvault/internal/store"
)

const searchRRFConstant = 60.0

type fusedSearchCandidate struct {
	result   store.DocumentSearchResult
	lexical  *store.DocumentSearchResult
	semantic *store.DocumentSearchResult
}

func fuseSearchResults(
	lexical, semantic []store.DocumentSearchResult, limit int,
) []store.DocumentSearchResult {
	byOccurrence := make(map[string]*fusedSearchCandidate, len(lexical)+len(semantic))
	for index := range lexical {
		row := lexical[index]
		candidate := byOccurrence[row.OccurrenceKey]
		if candidate == nil {
			candidate = &fusedSearchCandidate{}
			byOccurrence[row.OccurrenceKey] = candidate
		}
		if candidate.lexical == nil || row.Rank < candidate.lexical.Rank {
			rowCopy := row
			candidate.lexical = &rowCopy
		}
	}
	for index := range semantic {
		row := semantic[index]
		candidate := byOccurrence[row.OccurrenceKey]
		if candidate == nil {
			candidate = &fusedSearchCandidate{}
			byOccurrence[row.OccurrenceKey] = candidate
		}
		if candidate.semantic == nil || semanticResultLess(row, *candidate.semantic) {
			rowCopy := row
			candidate.semantic = &rowCopy
		}
	}

	candidates := make([]fusedSearchCandidate, 0, len(byOccurrence))
	for _, candidate := range byOccurrence {
		if candidate.semantic != nil {
			candidate.result = *candidate.semantic
			candidate.result.SemanticRank = candidate.semantic.SemanticRank
			candidate.result.FusionScore += reciprocalRank(candidate.semantic.SemanticRank)
			candidate.result.MatchedSignals = []string{"semantic"}
		} else {
			candidate.result = *candidate.lexical
		}
		if candidate.lexical != nil {
			candidate.result.LexicalRank = candidate.lexical.Rank
			candidate.result.FusionScore += reciprocalRank(candidate.lexical.Rank)
			candidate.result.MatchedSignals = slices.Clone(candidate.lexical.MatchedSignals)
			if candidate.semantic != nil {
				candidate.result.MatchedSignals = append(candidate.result.MatchedSignals, "semantic")
			}
		}
		candidates = append(candidates, *candidate)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].result.FusionScore != candidates[j].result.FusionScore {
			return candidates[i].result.FusionScore > candidates[j].result.FusionScore
		}
		return candidates[i].result.OccurrenceKey < candidates[j].result.OccurrenceKey
	})
	if limit > 0 && len(candidates) > limit {
		candidates = candidates[:limit]
	}
	results := make([]store.DocumentSearchResult, len(candidates))
	for index := range candidates {
		results[index] = candidates[index].result
		results[index].Rank = index + 1
	}
	return results
}

func semanticResultLess(left, right store.DocumentSearchResult) bool {
	if left.SemanticRank != right.SemanticRank {
		return left.SemanticRank < right.SemanticRank
	}
	if left.SemanticScore != right.SemanticScore {
		return left.SemanticScore > right.SemanticScore
	}
	return left.VectorToken < right.VectorToken
}

func reciprocalRank(rank int) float64 {
	if rank < 1 {
		return 0
	}
	return 1 / (searchRRFConstant + float64(rank))
}
