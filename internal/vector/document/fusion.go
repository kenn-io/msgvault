package document

import (
	"fmt"
	"slices"
	"sort"

	docembedding "go.kenn.io/docbank/document/embedding"
	"go.kenn.io/msgvault/internal/store"
)

type fusedSearchCandidate struct {
	result   store.DocumentSearchResult
	lexical  *store.DocumentSearchResult
	semantic *store.DocumentSearchResult
}

func fuseSearchResults(
	lexical, semantic []store.DocumentSearchResult, limit int,
) ([]store.DocumentSearchResult, bool, error) {
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
	lexicalCandidates := make([]docembedding.RankedCandidate, 0, len(byOccurrence))
	semanticCandidates := make([]docembedding.RankedCandidate, 0, len(byOccurrence))
	for _, candidate := range byOccurrence {
		if candidate.lexical != nil {
			lexicalCandidates = append(lexicalCandidates, docembedding.RankedCandidate{
				Key: candidate.lexical.OccurrenceKey, Rank: candidate.lexical.Rank,
			})
		}
		if candidate.semantic != nil {
			semanticCandidates = append(semanticCandidates, docembedding.RankedCandidate{
				Key: candidate.semantic.OccurrenceKey, Rank: candidate.semantic.SemanticRank,
				Score: candidate.semantic.SemanticScore,
			})
		}
	}
	sort.Slice(lexicalCandidates, func(i, j int) bool { return lexicalCandidates[i].Rank < lexicalCandidates[j].Rank })
	sort.Slice(semanticCandidates, func(i, j int) bool {
		if semanticCandidates[i].Rank != semanticCandidates[j].Rank {
			return semanticCandidates[i].Rank < semanticCandidates[j].Rank
		}
		return semanticCandidates[i].Key < semanticCandidates[j].Key
	})
	makeCandidateRanksStrict(lexicalCandidates)
	makeCandidateRanksStrict(semanticCandidates)
	fused, err := docembedding.FuseReciprocalRank(docembedding.FusionInput{
		Lexical:  docembedding.ScopedCandidates{Candidates: lexicalCandidates},
		Semantic: docembedding.ScopedCandidates{Candidates: semanticCandidates},
	}, limit)
	if err != nil {
		return nil, false, fmt.Errorf("fuse Docbank ranked candidates: %w", err)
	}
	results := make([]store.DocumentSearchResult, 0, len(fused.Candidates))
	for _, shared := range fused.Candidates {
		candidate := byOccurrence[shared.Key]
		if candidate.lexical != nil {
			candidate.result = *candidate.lexical
			candidate.result.LexicalRank = candidate.lexical.Rank
			candidate.result.MatchedSignals = slices.Clone(candidate.lexical.MatchedSignals)
			if candidate.semantic != nil {
				if candidate.result.PersonProvenance == nil {
					candidate.result.PersonProvenance = candidate.semantic.PersonProvenance
				}
				candidate.result.SemanticRank = candidate.semantic.SemanticRank
				candidate.result.SemanticScore = candidate.semantic.SemanticScore
				candidate.result.VectorToken = candidate.semantic.VectorToken
				candidate.result.VectorGenerationID = candidate.semantic.VectorGenerationID
				candidate.result.VectorGenerationFingerprint = candidate.semantic.VectorGenerationFingerprint
				candidate.result.VectorEmbeddingProfile = candidate.semantic.VectorEmbeddingProfile
				candidate.result.VectorModel = candidate.semantic.VectorModel
				candidate.result.VectorDimension = candidate.semantic.VectorDimension
				candidate.result.MatchedSignals = append(candidate.result.MatchedSignals, "semantic")
			}
		} else {
			candidate.result = *candidate.semantic
			candidate.result.SemanticRank = candidate.semantic.SemanticRank
			candidate.result.MatchedSignals = []string{"semantic"}
		}
		candidate.result.FusionScore = shared.Score
		candidate.result.Rank = shared.Rank
		results = append(results, candidate.result)
	}
	return results, fused.Truncated, nil
}

func makeCandidateRanksStrict(candidates []docembedding.RankedCandidate) {
	lastRank := 0
	for index := range candidates {
		if candidates[index].Rank <= lastRank {
			candidates[index].Rank = lastRank + 1
		}
		lastRank = candidates[index].Rank
	}
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
