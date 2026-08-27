package document

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personscope"
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

	got, truncated, err := fuseSearchResults(nil, semantic, 10)

	requirements.NoError(err)
	requirements.Len(got, 2)
	assertions.False(truncated)
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
	provenance := &personscope.Provenance{ParticipantIDs: []int64{7}}
	lexical := []store.DocumentSearchResult{
		{OccurrenceKey: "occ-b", Rank: 1, MatchedSignals: []string{"filename"}, PersonProvenance: provenance},
	}
	semantic := []store.DocumentSearchResult{
		{OccurrenceKey: "occ-a", SemanticRank: 1, SemanticScore: .9, VectorToken: "token-a"},
		{OccurrenceKey: "occ-b", SemanticRank: 2, SemanticScore: .8, VectorToken: "token-b"},
		{OccurrenceKey: "occ-b", SemanticRank: 3, SemanticScore: .7, VectorToken: "token-duplicate"},
	}

	got, truncated, err := fuseSearchResults(lexical, semantic, 10)

	requirements.NoError(err)
	requirements.Len(got, 2)
	assertions.False(truncated)
	assertions.Equal("occ-b", got[0].OccurrenceKey)
	assertions.Equal([]string{"filename", "semantic"}, got[0].MatchedSignals)
	assertions.Equal(1, got[0].LexicalRank)
	assertions.Equal(2, got[0].SemanticRank)
	assertions.Equal(provenance, got[0].PersonProvenance)
	assertions.InDelta(1.0/61.0+1.0/62.0, got[0].FusionScore, 1e-12)
	assertions.Equal("occ-a", got[1].OccurrenceKey)
	assertions.InDelta(1.0/61.0, got[1].FusionScore, 1e-12)
}

func TestFuseSearchResultsPreservesLaneRanksAndLexicalExcerpt(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	lexical := []store.DocumentSearchResult{
		{
			OccurrenceKey: "occ-a", Rank: 4, MatchedSignals: []string{"content"},
			ChunkKey: "lexical-chunk", ChunkOrdinal: 2, HeadingPath: []string{"Lexical"},
			FirstUnitIndex: 4, LastUnitIndex: 5,
			Excerpt: "matching lexical excerpt", HighlightStart: 9, HighlightEnd: 16,
		},
	}
	semantic := []store.DocumentSearchResult{
		{
			OccurrenceKey: "occ-a", SemanticRank: 7, SemanticScore: .9, VectorToken: "token-a",
			ChunkKey: "semantic-chunk", ChunkOrdinal: 8, HeadingPath: []string{"Semantic"},
			FirstUnitIndex: 12, LastUnitIndex: 13, Excerpt: "semantic excerpt",
		},
	}

	got, truncated, err := fuseSearchResults(lexical, semantic, 10)

	requirements.NoError(err)
	requirements.Len(got, 1)
	assertions.False(truncated)
	assertions.Equal(4, got[0].LexicalRank)
	assertions.Equal(7, got[0].SemanticRank)
	assertions.InDelta(1.0/64.0+1.0/67.0, got[0].FusionScore, 1e-12)
	assertions.Equal("matching lexical excerpt", got[0].Excerpt)
	assertions.Equal(9, got[0].HighlightStart)
	assertions.Equal(16, got[0].HighlightEnd)
	assertions.Equal("lexical-chunk", got[0].ChunkKey)
	assertions.Equal(2, got[0].ChunkOrdinal)
	assertions.Equal([]string{"Lexical"}, got[0].HeadingPath)
	assertions.Equal(4, got[0].FirstUnitIndex)
	assertions.Equal(5, got[0].LastUnitIndex)
	assertions.Equal("token-a", got[0].VectorToken)
}

func TestFuseSearchResultsBreaksSemanticTiesByOccurrenceIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	semantic := []store.DocumentSearchResult{
		{OccurrenceKey: "occ-z", SemanticRank: 1, SemanticScore: .5},
		{OccurrenceKey: "occ-a", SemanticRank: 1, SemanticScore: .5},
	}

	got, truncated, err := fuseSearchResults(nil, semantic, 10)

	require.NoError(err)
	require.Len(got, 2)
	assert.False(truncated)
	assert.Equal([]string{"occ-a", "occ-z"}, []string{got[0].OccurrenceKey, got[1].OccurrenceKey})
}

func TestFuseSearchResultsReportsDisjointCandidateOverflow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	lexical := []store.DocumentSearchResult{{OccurrenceKey: "occ-lexical", Rank: 1}}
	semantic := []store.DocumentSearchResult{{OccurrenceKey: "occ-semantic", SemanticRank: 1}}

	got, truncated, err := fuseSearchResults(lexical, semantic, 1)

	require.NoError(err)
	require.Len(got, 1)
	assert.True(truncated)
}
