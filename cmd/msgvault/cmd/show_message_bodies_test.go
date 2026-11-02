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
				name, text, html, snippet, want string
			}{
				{"HTML-only", "", "<p>Full delivery details</p>", "Short preview", "Full delivery details"},
				{"HTML-only without snippet", "", "<p>Full delivery details</p>", "", "Full delivery details"},
				{"plain text preferred", "Plain delivery details", "<p>HTML alternative</p>", "Short preview", "Plain delivery details"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					assert := assert.New(t)
					require := require.New(t)
					savedBodyOnly, savedStrip := showMessageBodyOnly, showMessageStripQuoted
					t.Cleanup(func() { showMessageBodyOnly, showMessageStripQuoted = savedBodyOnly, savedStrip })
					showMessageBodyOnly, showMessageStripQuoted = bodyOnly, false
					message := &query.MessageDetail{BodyText: tc.text, BodyHTML: tc.html, Snippet: tc.snippet}
					var output bytes.Buffer
					require.NoError(outputMessageText(&output, message))
					assert.Contains(output.String(), tc.want)
					assert.NotContains(output.String(), "<p>")
					assert.NotContains(output.String(), "Short preview")
					assert.NotContains(output.String(), "No body")
					assert.NotContains(output.String(), "HTML alternative")
					assert.Equal(tc.text, message.BodyText)
					assert.Equal(tc.html, message.BodyHTML)
				})
			}
		})
	}
}
