package chunkmatch

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
)

func TestBuild_ContextualChatSpanOmitsRenderedHeader(t *testing.T) {
	assert := assert.New(t)
	t.Parallel()

	const body = "Tuesday works"
	matches, truncated := Build("Synthetic header", body, vector.Config{}, []vector.ChunkHit{{
		ChunkIndex: 0, ChunkCharStart: 0, ChunkCharEnd: utf8.RuneCountInString(body),
		SourceBasis: vector.SourceBasisBody, Score: 0.9,
	}}, 0, 1, 200)

	require.Len(t, matches, 1)
	assert.Equal(body, matches[0].Snippet)
	assert.NotContains(matches[0].Snippet, "Synthetic header")
	assert.False(truncated)
}

func TestBuild_ContextualMeetingTurnPackUsesStoredBodySpan(t *testing.T) {
	assert := assert.New(t)
	t.Parallel()

	const body = "Roadmap sync\nWhen: 2026-08-09\nAttendees: Alice, Bob\n\nTranscript:\n[00:00] Alice: First point\n[00:30] Bob: Second point"
	const turns = "[00:00] Alice: First point\n[00:30] Bob: Second point"
	start, end := sourceRuneSpan(body, turns, 0)
	matches, _ := Build("Rendered meeting header", body, vector.Config{}, []vector.ChunkHit{{
		ChunkCharStart: start, ChunkCharEnd: end, SourceBasis: vector.SourceBasisBody, Score: 0.91,
	}}, 0, 5, 300)

	require.Len(t, matches, 1)
	assert.Equal(turns, matches[0].Snippet)
	assert.NotContains(matches[0].Snippet, "Rendered meeting header")
	assert.NotContains(matches[0].Snippet, "Attendees:")
}

func TestBuild_ContextualMeetingTitleDuplicateUsesStoredOccurrence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()

	const body = "Roadmap\nWhen: 2026-08-09\nAttendees: Alice\n\nRoadmap\n\nTranscript:\n[00:00] Alice: Done"
	start, end := sourceRuneSpan(body, "Roadmap", 1)
	matches, _ := Build("Roadmap", body, vector.Config{}, []vector.ChunkHit{{
		ChunkCharStart: start, ChunkCharEnd: end, SourceBasis: vector.SourceBasisBody, Score: 0.92,
	}}, 0, 5, 300)

	require.Len(matches, 1)
	require.NotNil(matches[0].CharOffset)
	assert.Equal(strings.LastIndex(body, "Roadmap"), *matches[0].CharOffset)
	assert.Equal("Roadmap", matches[0].Snippet)
}

func TestBuild_ContextualUnicodeSpanUsesRuneOffsets(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()

	const body = "zero\n🍣 café résumé\nend"
	const passage = "🍣 café résumé"
	start, end := sourceRuneSpan(body, passage, 0)
	matches, _ := Build("Unicode subject", body, vector.Config{}, []vector.ChunkHit{{
		ChunkCharStart: start, ChunkCharEnd: end, SourceBasis: vector.SourceBasisBody, Score: 0.88,
	}}, 0, 5, 300)

	require.Len(matches, 1)
	require.NotNil(matches[0].CharOffset)
	assert.Equal(strings.Index(body, passage), *matches[0].CharOffset)
	assert.Equal(passage, matches[0].Snippet)
}

func TestBuild_ContextualRepeatedTextUsesStoredOccurrence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()

	const body = "repeat\nmiddle\nrepeat"
	start, end := sourceRuneSpan(body, "repeat", 1)
	matches, _ := Build("", body, vector.Config{}, []vector.ChunkHit{{
		ChunkCharStart: start, ChunkCharEnd: end, SourceBasis: vector.SourceBasisBody, Score: 0.87,
	}}, 0, 5, 300)

	require.Len(matches, 1)
	require.NotNil(matches[0].CharOffset)
	assert.Equal(strings.LastIndex(body, "repeat"), *matches[0].CharOffset)
	assert.Equal(3, *matches[0].Line)
}

func TestBuild_ContextualTransformBeforeDuplicateDoesNotGuessRawLocation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()

	const body = "prefix\r\naaaaa"
	processed, _ := embed.Preprocess("", body, 0, preprocessConfig(vector.Config{}))
	// CRLF normalization shifts the second preprocessed occurrence to the
	// raw first occurrence's rune position. That positional match is unsafe.
	start := utf8.RuneCountInString("prefix\na")
	end := start + utf8.RuneCountInString("aaaa")
	require.Equal("prefix\naaaaa", processed)
	matches, _ := Build("Synthetic subject", body, vector.Config{}, []vector.ChunkHit{{
		ChunkCharStart: start, ChunkCharEnd: end, SourceBasis: vector.SourceBasisBody, Score: 0.86,
	}}, 0, 5, 300)

	require.Len(matches, 1)
	assert.Equal("aaaa", matches[0].Snippet)
	assert.Nil(matches[0].CharOffset)
	assert.Nil(matches[0].Line)
}

func TestBuild_OrdinaryDuplicateTextPreservesLegacyUnknownLocation(t *testing.T) {
	assert := assert.New(t)
	t.Parallel()

	const subject = "Repeated passage"
	const body = "repeat\nrepeat"
	processed, _ := embed.Preprocess(subject, body, 0, preprocessConfig(vector.Config{}))
	start, end := sourceRuneSpan(processed, "repeat", 1)
	matches, _ := Build(subject, body, vector.Config{}, []vector.ChunkHit{{
		ChunkCharStart: start, ChunkCharEnd: end,
		SourceBasis: vector.SourceBasisSubjectBody, Score: 0.85,
	}}, 0, 5, 300)

	require.Len(t, matches, 1)
	assert.Equal("repeat", matches[0].Snippet)
	assert.Nil(matches[0].CharOffset)
	assert.Nil(matches[0].Line)
}

func TestBuild_OrdinarySourceBasisPreservesLegacySubjectBodyOutput(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()

	const subject = "Quarterly plan"
	const body = "first line\nlegacy body passage"
	processed, _ := embed.Preprocess(subject, body, 0, preprocessConfig(vector.Config{}))
	start, end := sourceRuneSpan(processed, "legacy body passage", 0)
	matches, _ := Build(subject, body, vector.Config{}, []vector.ChunkHit{
		{ChunkCharStart: 0, ChunkCharEnd: utf8.RuneCountInString("Subject: Quarterly plan"), SourceBasis: vector.SourceBasisSubjectBody, Score: 0.9},
		{ChunkCharStart: start, ChunkCharEnd: end, SourceBasis: vector.SourceBasisSubjectBody, Score: 0.8},
	}, 0, 5, 300)

	require.Len(matches, 2)
	assert.Equal("Subject: Quarterly plan", matches[0].Snippet)
	assert.Nil(matches[0].CharOffset)
	assert.Equal("legacy body passage", matches[1].Snippet)
	require.NotNil(matches[1].CharOffset)
	assert.Equal(strings.Index(body, "legacy body passage"), *matches[1].CharOffset)
}

func TestBuild_ZeroValueBasisIsPlainLegacyFallback(t *testing.T) {
	t.Parallel()

	const subject = "Plain subject"
	const body = "plain body"
	processed, _ := embed.Preprocess(subject, body, 0, preprocessConfig(vector.Config{}))
	start, end := sourceRuneSpan(processed, body, 0)
	matches, _ := Build(subject, body, vector.Config{}, []vector.ChunkHit{{
		ChunkCharStart: start, ChunkCharEnd: end, Score: 0.8,
	}}, 0, 5, 300)

	require.Len(t, matches, 1)
	assert.Equal(t, body, matches[0].Snippet)
}

func TestBuild_ContextualBodyBasisUsesExactPreprocessTransforms(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "CRLF normalization", body: "first\r\nsecond", want: "first\nsecond"},
		{name: "HTML and whitespace", body: "<p>alpha</p>   beta", want: "alpha beta"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			processed, _ := embed.Preprocess("", tt.body, 0, preprocessConfig(vector.Config{}))
			matches, _ := Build("Synthetic subject", tt.body, vector.Config{}, []vector.ChunkHit{{
				ChunkCharStart: 0, ChunkCharEnd: utf8.RuneCountInString(processed),
				SourceBasis: vector.SourceBasisBody, Score: 0.8,
			}}, 0, 5, 300)

			require.Len(t, matches, 1)
			assert.Equal(t, tt.want, matches[0].Snippet)
			assert.NotContains(t, matches[0].Snippet, "Synthetic subject")
		})
	}
}

func TestBuild_InvalidSourceSpansAreSkipped(t *testing.T) {
	t.Parallel()

	hits := []vector.ChunkHit{
		{ChunkCharStart: -1, ChunkCharEnd: 4, SourceBasis: vector.SourceBasisBody, Score: 0.9},
		{ChunkCharStart: 5, ChunkCharEnd: 3, SourceBasis: vector.SourceBasisBody, Score: 0.9},
		{ChunkCharStart: 2, ChunkCharEnd: 2, SourceBasis: vector.SourceBasisBody, Score: 0.9},
		{ChunkCharStart: 0, ChunkCharEnd: 500, SourceBasis: vector.SourceBasisBody, Score: 0.9},
		{ChunkCharStart: 0, ChunkCharEnd: 4, SourceBasis: vector.SourceBasis(99), Score: 0.9},
	}
	matches, truncated := Build("Never leak this rendered header", "body", vector.Config{}, hits, 0, 5, 300)

	assert.Empty(t, matches)
	assert.False(t, truncated)
}

func TestBuild_MultipleContextualHitsUseTheirStoredSourceSpans(t *testing.T) {
	assert := assert.New(t)
	t.Parallel()

	const body = "alpha beta gamma"
	matches, truncated := Build("header", body, vector.Config{}, []vector.ChunkHit{
		{ChunkIndex: 2, ChunkCharStart: 11, ChunkCharEnd: 16, SourceBasis: vector.SourceBasisBody, Score: 0.9},
		{ChunkIndex: 0, ChunkCharStart: 0, ChunkCharEnd: 5, SourceBasis: vector.SourceBasisBody, Score: 0.8},
		{ChunkIndex: 1, ChunkCharStart: 6, ChunkCharEnd: 10, SourceBasis: vector.SourceBasisBody, Score: 0.2},
	}, 0.5, 2, 4)

	require.Len(t, matches, 2)
	assert.Equal("gamm", matches[0].Snippet)
	assert.Equal("alph", matches[1].Snippet)
	assert.False(truncated)
}

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

func sourceRuneSpan(source, passage string, occurrence int) (int, int) {
	byteStart := -1
	remainderStart := 0
	for i := 0; i <= occurrence; i++ {
		relative := strings.Index(source[remainderStart:], passage)
		if relative < 0 {
			return -1, -1
		}
		byteStart = remainderStart + relative
		remainderStart = byteStart + len(passage)
	}
	start := utf8.RuneCountInString(source[:byteStart])
	return start, start + utf8.RuneCountInString(passage)
}
