package cmd

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil/messagebody"
)

func TestReadableMessageBody(t *testing.T) {
	for _, tc := range messagebody.ReadableCases {
		t.Run(tc.Name, func(t *testing.T) { assert.Equal(t, tc.Want, readableMessageBody(tc.Body)) })
	}
}

func TestMessageBodyOnlyOutput(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	savedBodyOnly, savedStrip := showMessageBodyOnly, showMessageStripQuoted
	t.Cleanup(func() { showMessageBodyOnly = savedBodyOnly; showMessageStripQuoted = savedStrip })
	showMessageBodyOnly, showMessageStripQuoted = true, true
	done := captureStdout(t)
	err := outputMessageText(os.Stdout, &query.MessageDetail{Subject: "hidden header", BodyText: "Current reply\n> old text"})
	got := done()
	require.NoError(err)
	assert.Equal("Current reply\n", got)
}

func TestMessageJSONStripQuotedPreview(t *testing.T) {
	for _, tc := range []struct{ name, body, snippet, want string }{
		{"quoted body", "> old text", "stale preview", "stale preview"},
		{"bodyless preview", "", "Current preview\n> old text", "Current preview\n> old text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			saved := showMessageStripQuoted
			t.Cleanup(func() { showMessageStripQuoted = saved })
			showMessageStripQuoted = true
			message := &query.MessageDetail{BodyText: tc.body, Snippet: tc.snippet}
			done := captureStdout(t)
			err := outputMessageJSON(os.Stdout, message)
			output := done()
			require.NoError(err)
			var fields map[string]any
			require.NoError(json.Unmarshal([]byte(output), &fields))
			assert.Equal(tc.want, fields["snippet"])
			assert.Empty(fields["body_text"])
			assert.Equal(tc.snippet, message.Snippet, "display cleanup does not mutate the archive")
		})
	}
}

func TestMessageTextStripQuotedDoesNotRestorePreview(t *testing.T) {
	for _, bodyOnly := range []bool{false, true} {
		t.Run(strconv.FormatBool(bodyOnly), func(t *testing.T) {
			savedBodyOnly, savedStrip := showMessageBodyOnly, showMessageStripQuoted
			t.Cleanup(func() { showMessageBodyOnly = savedBodyOnly; showMessageStripQuoted = savedStrip })
			showMessageBodyOnly, showMessageStripQuoted = bodyOnly, true
			done := captureStdout(t)
			err := outputMessageText(os.Stdout, &query.MessageDetail{BodyText: "> old text", Snippet: "stale preview"})
			output := done()
			require.NoError(t, err)
			assert.NotContains(t, output, "old text")
			assert.NotContains(t, output, "stale preview")
		})
	}
}
