package document

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestFuseSearchResultsCollapsesDuplicateSemanticChunksPerOccurrence(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	semantic := []store.DocumentSearchResult{
		{OccurrenceKey: "occ-a", SemanticRank: 1, SemanticScore: .9, VectorToken: "token-a"},
		{OccurrenceKey: "occ-a", SemanticRank: 2, SemanticScore: .8, VectorToken: "token-duplicate"},
		{OccurrenceKey: "occ-b", SemanticRank: 3, SemanticScore: .7, VectorToken: "token-b"},
	}

	got := fuseSearchResults(nil, semantic, 10)

	requirements.Len(got, 2)
	assertions.Equal("occ-a", got[0].OccurrenceKey)
	assertions.Equal("token-a", got[0].VectorToken)
	assertions.Equal(1, got[0].SemanticRank)
	assertions.Equal([]string{"semantic"}, got[0].MatchedSignals)
	assertions.Equal("occ-b", got[1].OccurrenceKey)
	assertions.Equal(1, got[0].Rank)
}

func TestFuseSearchResultsUsesOneRRFContributionPerSignal(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	lexical := []store.DocumentSearchResult{
		{OccurrenceKey: "occ-b", Rank: 1, MatchedSignals: []string{"filename"}},
	}
	semantic := []store.DocumentSearchResult{
		{OccurrenceKey: "occ-a", SemanticRank: 1, SemanticScore: .9, VectorToken: "token-a"},
		{OccurrenceKey: "occ-b", SemanticRank: 2, SemanticScore: .8, VectorToken: "token-b"},
		{OccurrenceKey: "occ-b", SemanticRank: 3, SemanticScore: .7, VectorToken: "token-duplicate"},
	}

	got := fuseSearchResults(lexical, semantic, 10)

	requirements.Len(got, 2)
	assertions.Equal("occ-b", got[0].OccurrenceKey)
	assertions.Equal([]string{"filename", "semantic"}, got[0].MatchedSignals)
	assertions.Equal(1, got[0].LexicalRank)
	assertions.Equal(2, got[0].SemanticRank)
	assertions.InDelta(1.0/61.0+1.0/62.0, got[0].FusionScore, 1e-12)
	assertions.Equal("occ-a", got[1].OccurrenceKey)
	assertions.InDelta(1.0/61.0, got[1].FusionScore, 1e-12)
}

func TestFuseSearchResultsBreaksSemanticTiesByOccurrenceIdentity(t *testing.T) {
	semantic := []store.DocumentSearchResult{
		{OccurrenceKey: "occ-z", SemanticRank: 1, SemanticScore: .5},
		{OccurrenceKey: "occ-a", SemanticRank: 1, SemanticScore: .5},
	}

	got := fuseSearchResults(nil, semantic, 10)

	require.Len(t, got, 2)
	assert.Equal(t, []string{"occ-a", "occ-z"}, []string{got[0].OccurrenceKey, got[1].OccurrenceKey})
}
