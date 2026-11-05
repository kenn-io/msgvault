package cmd

import (
	"bytes"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
)

func TestOutputMessageTextBodyAlternatives(t *testing.T) {
	for _, bodyOnly := range []bool{false, true} {
		t.Run(strconv.FormatBool(bodyOnly), func(t *testing.T) {
			for _, tc := range []struct {
				name, html, snippet, want string
				wantSnippetLabel          bool
			}{
				{"HTML without stored plain text", "<p>Full delivery details</p>", "Short preview", "Full delivery details", false},
				{"HTML without readable text", "<img src=x.png>", "Photo from Bob", "Photo from Bob", true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					assert := assert.New(t)
					require := require.New(t)
					savedBodyOnly, savedStrip := showMessageBodyOnly, showMessageStripQuoted
					t.Cleanup(func() { showMessageBodyOnly, showMessageStripQuoted = savedBodyOnly, savedStrip })
					showMessageBodyOnly, showMessageStripQuoted = bodyOnly, false
					message := &query.MessageDetail{BodyHTML: tc.html, Snippet: tc.snippet}
					var output bytes.Buffer
					require.NoError(outputMessageText(&output, message))
					assert.Contains(output.String(), tc.want)
					if !tc.wantSnippetLabel {
						assert.NotContains(output.String(), "<p>")
						assert.NotContains(output.String(), "Short preview")
					}
					if tc.wantSnippetLabel && !bodyOnly {
						assert.Contains(output.String(), "[No body text available. Snippet: Photo from Bob]")
					} else {
						assert.NotContains(output.String(), "No body")
					}
				})
			}
		})
	}
}
