package mcp

import (
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

func TestChunkHitsToMatches_ordersByScoreAndMapsOffsets(t *testing.T) {
	t.Parallel()
	assert := assertpkg.New(t)
	require := requirepkg.New(t)

	preprocessed := "Subject: Hello\n\nFirst paragraph about budgets.\n\nSecond paragraph."
	body := "First paragraph about budgets.\n\nSecond paragraph."
	prefixRunes := subjectPrefixRuneCount("Hello")

	hits := []vector.ChunkHit{
		{ChunkIndex: 0, ChunkCharStart: prefixRunes, ChunkCharEnd: prefixRunes + 28, Score: 0.9},
		{ChunkIndex: 1, ChunkCharStart: prefixRunes + 30, ChunkCharEnd: prefixRunes + 50, Score: 0.7},
	}

	matches, truncated := chunkHitsToMatches(preprocessed, body, prefixRunes, hits, 0, 5)
	require.Len(matches, 2)
	require.NotNil(matches[0].Score)
	assert.InDelta(0.9, *matches[0].Score, 0.001)
	assert.Contains(matches[0].Snippet, "budget")
	assert.Equal(0, matches[0].CharOffset)
	assert.False(truncated)
}

func TestChunkHitsToMatches_minScoreAndTruncation(t *testing.T) {
	t.Parallel()
	assert := assertpkg.New(t)
	require := requirepkg.New(t)

	body := "alpha beta gamma delta"
	hits := []vector.ChunkHit{
		{ChunkIndex: 0, ChunkCharStart: 0, ChunkCharEnd: 5, Score: 0.2},
		{ChunkIndex: 1, ChunkCharStart: 6, ChunkCharEnd: 10, Score: 0.8},
		{ChunkIndex: 2, ChunkCharStart: 11, ChunkCharEnd: 16, Score: 0.6},
	}

	matches, truncated := chunkHitsToMatches(body, body, 0, hits, 0.5, 1)
	require.Len(matches, 1)
	assert.InDelta(0.8, *matches[0].Score, 0.001)
	assert.True(truncated)
}

func TestExtractContextMatches_keywordShape(t *testing.T) {
	t.Parallel()
	assert := assertpkg.New(t)
	require := requirepkg.New(t)

	body := "Line one\nLine two with TARGET here\nLine three"
	matches := extractContextMatches(body, []string{"TARGET"}, 80)
	require.NotEmpty(matches)
	assert.Contains(matches[0].Snippet, "TARGET")
	assert.Positive(matches[0].CharOffset)
	assert.Equal(2, matches[0].Line)
	assert.Nil(matches[0].Score)
}
