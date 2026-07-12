package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/chunkmatch"
)

func TestChunkHitsToMatches_ordersByScoreAndMapsOffsets(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)

	body := "First paragraph about budgets.\n\nSecond paragraph."
	prefixRunes := 16

	hits := []vector.ChunkHit{
		{ChunkIndex: 0, ChunkCharStart: prefixRunes, ChunkCharEnd: prefixRunes + 28, Score: 0.9},
		{ChunkIndex: 1, ChunkCharStart: prefixRunes + 30, ChunkCharEnd: prefixRunes + 50, Score: 0.7},
	}

	chunkMatches, truncated := chunkmatch.Build("Hello", body, vector.Config{}, hits, 0, 5, searchContextChars)
	matches := messageMatchesFromChunks(chunkMatches)
	require.Len(matches, 2)
	require.NotNil(matches[0].Score)
	assert.InDelta(0.9, *matches[0].Score, 0.001)
	assert.Contains(matches[0].Snippet, "budget")
	require.NotNil(matches[0].CharOffset)
	assert.Equal(0, *matches[0].CharOffset)
	assert.False(truncated)
}

func TestChunkHitsToMatches_minScoreAndTruncation(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)

	body := "alpha beta gamma delta"
	hits := []vector.ChunkHit{
		{ChunkIndex: 0, ChunkCharStart: 0, ChunkCharEnd: 5, Score: 0.2},
		{ChunkIndex: 1, ChunkCharStart: 6, ChunkCharEnd: 10, Score: 0.8},
		{ChunkIndex: 2, ChunkCharStart: 11, ChunkCharEnd: 16, Score: 0.6},
	}

	chunkMatches, truncated := chunkmatch.Build("", body, vector.Config{}, hits, 0.5, 1, searchContextChars)
	matches := messageMatchesFromChunks(chunkMatches)
	require.Len(matches, 1)
	assert.InDelta(0.8, *matches[0].Score, 0.001)
	assert.True(truncated)
}

func TestExtractContextMatches_keywordShape(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)

	body := "Line one\nLine two with TARGET here\nLine three"
	matches := extractContextMatches(body, []string{"TARGET"}, 80)
	require.NotEmpty(matches)
	assert.Contains(matches[0].Snippet, "TARGET")
	require.NotNil(matches[0].CharOffset)
	require.NotNil(matches[0].Line)
	assert.Positive(*matches[0].CharOffset)
	assert.Equal(2, *matches[0].Line)
	assert.Nil(matches[0].Score)
}
