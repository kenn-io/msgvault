package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
)

func TestSearchInMessageBodyAlternatives(t *testing.T) {
	for _, tc := range []struct {
		name, text, html, format string
	}{
		{"HTML without stored plain text", "", "<p>Résumé</p>\n<p>delivery package</p>", bodyFormatHTML},
		{"plain text preferred", "plain delivery", "<p>HTML delivery</p>", bodyFormatText},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			h := newTestHandlers(&querytest.MockEngine{
				Messages: map[int64]*query.MessageDetail{
					42: {ID: 42, BodyText: tc.text, BodyHTML: tc.html},
				},
			})
			matches := runTool[paginatedInMessageMatches](t, ToolSearchInMessage, h.searchInMessage, map[string]any{
				"id": float64(42), "query": "delivery",
			})
			require.Len(matches.Data, 1)
			require.NotNil(matches.Data[0].CharOffset)
			// Use the returned byte offset in the real automatic body window.
			// HTML must stay raw here so offsets include markup and UTF-8 bytes.
			window := runTool[getMessageResponse](t, ToolGetMessage, h.getMessage, map[string]any{
				"id": float64(42), "offset": float64(*matches.Data[0].CharOffset), "max_chars": float64(len("delivery")),
			})
			assert.Equal(tc.format, window.BodyFormat)
			assert.Equal("delivery", window.BodyText+window.BodyHTML)
		})
	}
}
