package query

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMarkedFragmentContext(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	startMarker := "__start__"
	endMarker := "__end__"
	ellipsisMarker := "__ellipsis__"
	marked := ellipsisMarker + strings.Repeat("quoted history ", 30) +
		startMarker + "needle" + endMarker + " answer" + ellipsisMarker

	snippets, truncated, err := markedFragmentContext(
		context.Background(), marked, startMarker, endMarker, ellipsisMarker,
	)
	require.NoError(err, "marked fragment context")
	require.Len(snippets, 1)
	assert.Contains(snippets[0], "needle")
	assert.LessOrEqual(len(snippets[0]), MessageBodyContextSnippetBytes)
	assert.True(utf8.ValidString(snippets[0]))
	assert.True(truncated, "native ellipses advertise omitted body text")
}

func TestMarkedFragmentContextHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := markedFragmentContext(ctx, "body", "<s>", "</s>", "...")
	require.ErrorIs(t, err, context.Canceled)
}

func TestBodyContextGuardRejectsArtificialTokenBoundary(t *testing.T) {
	body := strings.Repeat("x", messageBodyContextChunkCoreBytes+100)
	chunks := makeBodyContextChunks(1, body, 1)
	require.GreaterOrEqual(t, len(chunks), 2)

	second := chunks[1]
	state := bodyContextSourceState{chunked: true}
	assert.False(t, bodyContextSpanIsSafe(second, bodyContextSpan{
		start: 0,
		end:   10,
	}, state), "a token cut at the left guard cannot become a context match")
	assert.True(t, bodyContextSpanIsSafe(second, bodyContextSpan{
		start: second.coreStart - second.start,
		end:   second.coreStart - second.start + 10,
	}, state), "an interior match owned by the core is safe")
}

func TestValidBodyContextPrefixIsRuneSafe(t *testing.T) {
	prefix, truncated, err := validBodyContextPrefix([]byte("abcéz"), 4)
	require.NoError(t, err)
	assert.Equal(t, "abc", prefix)
	assert.True(t, truncated)
}

func TestBodyContextSnippetsCapsDistantMatches(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var body strings.Builder
	spans := make([]bodyContextSpan, 0, MessageBodyContextMaxSnippets+1)
	for range MessageBodyContextMaxSnippets + 1 {
		start := body.Len()
		body.WriteString("needle")
		spans = append(spans, bodyContextSpan{start: start, end: body.Len()})
		body.WriteString(strings.Repeat(" padding", MessageBodyContextSnippetBytes))
	}

	snippets, truncated := bodyContextSnippets(body.String(), spans)
	require.Len(snippets, MessageBodyContextMaxSnippets)
	assert.True(truncated)
	for _, snippet := range snippets {
		assert.LessOrEqual(len(snippet), MessageBodyContextSnippetBytes)
		assert.True(utf8.ValidString(snippet))
	}
}
