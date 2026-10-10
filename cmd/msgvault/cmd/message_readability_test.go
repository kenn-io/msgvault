package cmd

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
)

func TestReadableMessageBody(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"dated authored fragment", "On May 1, 2026 we agreed to ship\nAlice <alice@example.com> wrote:\n> old", "On May 1, 2026 we agreed to ship\nAlice <alice@example.com> wrote:"},
		{"complete URL", "https://example.com/doc?q=" + strings.Repeat("x", 200) + "#section", "https://example.com/doc?q=" + strings.Repeat("x", 200) + "#section"},
		{"quote lines", "Current reply\n> old reply\n\nMore current text", "Current reply\n\nMore current text"},
		{"reply history", "Current reply\n\nOn Tue, Oct 1, 2024, Example Sender wrote:\n> Old reply\n> old signature", "Current reply"},
		{"unquoted history keeps its header", "Current reply\nOn Tue, Oct 1, 2024, Example Sender wrote:\nUnmarked text", "Current reply\nOn Tue, Oct 1, 2024, Example Sender wrote:\nUnmarked text"},
		{"dated prose without quotes", "On May 1, 2026 we wrote:\nRelease notes", "On May 1, 2026 we wrote:\nRelease notes"},
		{"wrapped bottom posted", "On Tue, Oct 1, 2024,\nExample Sender <sender@example.com> wrote:\n> Old reply\n\nCurrent reply", "On Tue, Oct 1, 2024,\nExample Sender <sender@example.com> wrote:\n\nCurrent reply"},
		{"bottom posted", "On Tue, Oct 1, 2024, Example Sender wrote:\n> Old reply\n\nCurrent reply", "Current reply"},
		{"inline reply", "First answer\nOn Tue, Oct 1, 2024, Example Sender wrote:\n> Old reply\nSecond answer\n> Another old line\nThird answer", "First answer\nSecond answer\nThird answer"},
		{"signature", "Current reply\n-- \nExample Sender\nhttps://example.com", "Current reply"},
		{"ordinary prose", "On this project we wrote:\nA useful paragraph", "On this project we wrote:\nA useful paragraph"},
		{"weekday prose", "On Monday we wrote:\nA useful paragraph", "On Monday we wrote:\nA useful paragraph"},
		{"month prefix prose", "On marketing we wrote:\nA useful paragraph", "On marketing we wrote:\nA useful paragraph"},
		{"prose before header", "Thanks.\nOn Monday we agreed to ship.\nOn Mon, Jan 5, 2026, Example Sender <sender@example.com> wrote:\n> old", "Thanks.\nOn Monday we agreed to ship."},
		{"dated prose before lowercase header", "On May 1, 2026 we agreed to ship.\non Tue, May 5, 2026, Sender wrote:\n> old reply", "On May 1, 2026 we agreed to ship."},
		{"bare dashes in prose", "Agenda\n--\nItem one", "Agenda\n--\nItem one"},
		{"dated sentence above a sender line", "On Monday at 10:00 we shipped the fix.\nAlice wrote:\n> q", "On Monday at 10:00 we shipped the fix.\nAlice wrote:"},
		{"address in prose above speaker", "On Jan 5, 2026 the team met with Carol <carol@example.com>.\nShe wrote:\n> excerpt\nMy reply below the excerpt.", "On Jan 5, 2026 the team met with Carol <carol@example.com>.\nShe wrote:\nMy reply below the excerpt."},
		{"address wrapped inside brackets", "Reply\nOn Tue, Oct 1, 2024 at 10:00 AM John Doe <\njohn@example.com> wrote:\n> old", "Reply\nOn Tue, Oct 1, 2024 at 10:00 AM John Doe <\njohn@example.com> wrote:"},
		{"gmail wrapped header", "Reply\nOn Tue, Oct 1, 2024 at 10:00 AM Example Sender <sender@example.com>\nwrote:\n> old", "Reply\nOn Tue, Oct 1, 2024 at 10:00 AM Example Sender <sender@example.com>\nwrote:"},
		{"sent from my in prose", "Sent from my laptop yesterday.\n- ship v2", "Sent from my laptop yesterday.\n- ship v2"},
		{"URL punctuation", "(See https://example.com/doc?id=" + strings.Repeat("x", 200) + "). Thanks", "(See https://example.com/doc?id=" + strings.Repeat("x", 200) + "). Thanks"},
	} {
		t.Run(tc.name, func(t *testing.T) { assert.Equal(t, tc.want, readableMessageBody(tc.body)) })
	}
}

func TestMessageBodyOnlyOutput(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	savedBodyOnly, savedStrip := showMessageBodyOnly, showMessageStripQuoted
	t.Cleanup(func() { showMessageBodyOnly = savedBodyOnly; showMessageStripQuoted = savedStrip })
	showMessageBodyOnly, showMessageStripQuoted = true, true
	done := captureStdout(t)
	err := outputMessageText(&query.MessageDetail{Subject: "hidden header", BodyText: "Current reply\n> old text"})
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
			err := outputMessageJSON(message)
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
			err := outputMessageText(&query.MessageDetail{BodyText: "> old text", Snippet: "stale preview"})
			output := done()
			require.NoError(t, err)
			assert.NotContains(t, output, "old text")
			assert.NotContains(t, output, "stale preview")
		})
	}
}
