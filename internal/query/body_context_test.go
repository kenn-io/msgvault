package query

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mime"
)

func TestMarkedFragmentContext(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := markedFragmentContext(ctx, "body", "<s>", "</s>", "...")
	require.ErrorIs(t, err, context.Canceled)
}

func TestBodyContextGuardRejectsArtificialTokenBoundary(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	prefix, truncated, err := validBodyContextPrefix([]byte("abcéz"), 4)
	require.NoError(t, err)
	assert.Equal(t, "abc", prefix)
	assert.True(t, truncated)
}

func TestBodyContextHiddenHTMLPrefixBoundaryNeedsVisibleSeparator(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                 string
		html                 string
		wantVisibleSeparator bool
		wantSpanAtEndIsSafe  bool
	}{
		{
			name:                 "hidden content can join the preceding token",
			html:                 "needle<script>truncated hidden content",
			wantVisibleSeparator: false,
			wantSpanAtEndIsSafe:  false,
		},
		{
			name:                 "visible space completes the preceding token",
			html:                 "needle <script>truncated hidden content",
			wantVisibleSeparator: true,
			wantSpanAtEndIsSafe:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plain, hiddenAtEnd, visibleSeparatorAtEnd := mime.StripHTMLPrefixWithBoundaryState(tc.html)
			require.True(t, hiddenAtEnd)
			assert.Equal(t, "needle", plain)
			assert.Equal(t, tc.wantVisibleSeparator, visibleSeparatorAtEnd)

			state := bodyContextSourceState{spanBoundaryTruncated: hiddenAtEnd && !visibleSeparatorAtEnd}
			chunk := bodyContextChunk{
				body: plain, end: len(plain), coreEnd: len(plain),
			}
			assert.Equal(t, tc.wantSpanAtEndIsSafe, bodyContextSpanIsSafe(
				chunk, bodyContextSpan{start: 0, end: len(plain)}, state,
			))
		})
	}
}

func TestLoadBodyContextChunksSkipsOversizedBodies(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, bodyText, bodyHTML string
	}{
		{name: "plain text", bodyText: strings.Repeat("large body ", 32)},
		{name: "html", bodyHTML: strings.Repeat("<p>large body</p>", 32)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := metadataSearchDB(t)
			_, err := db.Exec(`INSERT INTO message_bodies(message_id, body_text, body_html) VALUES (1, ?, ?)`,
				tc.bodyText, tc.bodyHTML)
			require.NoError(t, err)

			states, _, bodies, err := NewSQLiteEngine(db).loadBodyContextChunks(
				t.Context(), []int64{1}, 32,
			)
			require.NoError(t, err)
			assert.Empty(t, bodies[1])
			assert.True(t, states[1].scanTruncated)
		})
	}
}

func TestBodyContextSnippetsCapsDistantMatches(t *testing.T) {
	t.Parallel()
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
