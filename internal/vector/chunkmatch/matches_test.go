package chunkmatch

import (
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
)

func TestBuildPreservesRawBodyOffsetZero(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)

	body := "alpha beta"
	matches, truncated := Build("", body, vector.Config{}, []vector.ChunkHit{
		{ChunkCharStart: 0, ChunkCharEnd: utf8.RuneCountInString(body), Score: 0.9},
	}, 0, 5, 300)

	require.Len(matches, 1)
	require.NotNil(matches[0].CharOffset)
	require.NotNil(matches[0].Line)
	assert.Equal(0, *matches[0].CharOffset)
	assert.Equal(1, *matches[0].Line)
	assert.False(truncated)
}

func TestBuildOmitsLocationWhenPreprocessingRewritesBody(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)

	body := "alpha   beta"
	processed, _ := embed.Preprocess("", body, 0, preprocessConfig(vector.Config{}))
	matches, _ := Build("", body, vector.Config{}, []vector.ChunkHit{
		{ChunkCharStart: 0, ChunkCharEnd: utf8.RuneCountInString(processed), Score: 0.8},
	}, 0, 5, 300)

	require.Len(t, matches, 1)
	assert.Nil(matches[0].CharOffset)
	assert.Nil(matches[0].Line)
	assert.Equal("alpha beta", matches[0].Snippet)
}

func TestBuildOmitsLocationForSubjectChunk(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)

	processed, _ := embed.Preprocess("Quarterly plan", "body text", 0, preprocessConfig(vector.Config{}))
	matches, _ := Build("Quarterly plan", "body text", vector.Config{}, []vector.ChunkHit{
		{ChunkCharStart: 0, ChunkCharEnd: min(12, utf8.RuneCountInString(processed)), Score: 0.7},
	}, 0, 5, 300)

	require.Len(t, matches, 1)
	assert.Nil(matches[0].CharOffset)
	assert.Nil(matches[0].Line)
	assert.Contains(matches[0].Snippet, "Subject:")
}

func TestBuildLocatesUniqueExactBodyChunk(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)

	body := "first line\nunique semantic passage\nlast line"
	prefix := "first line\n"
	chunk := "unique semantic passage"
	start := utf8.RuneCountInString(prefix)
	matches, _ := Build("", body, vector.Config{}, []vector.ChunkHit{
		{ChunkCharStart: start, ChunkCharEnd: start + utf8.RuneCountInString(chunk), Score: 0.95},
	}, 0, 5, 300)

	require.Len(matches, 1)
	require.NotNil(matches[0].CharOffset)
	require.NotNil(matches[0].Line)
	assert.Equal(len(prefix), *matches[0].CharOffset)
	assert.Equal(2, *matches[0].Line)
}

func TestBuildFiltersExcerptsByMinScoreAndReportsTruncation(t *testing.T) {
	t.Parallel()

	body := "alpha beta gamma"
	matches, truncated := Build("", body, vector.Config{}, []vector.ChunkHit{
		{ChunkIndex: 0, ChunkCharStart: 0, ChunkCharEnd: 5, Score: 0.9},
		{ChunkIndex: 1, ChunkCharStart: 6, ChunkCharEnd: 10, Score: 0.7},
		{ChunkIndex: 2, ChunkCharStart: 11, ChunkCharEnd: 16, Score: 0.2},
	}, 0.5, 1, 300)

	require.Len(t, matches, 1)
	assert.InDelta(t, 0.9, matches[0].Score, 0.001)
	assert.True(t, truncated)
}
