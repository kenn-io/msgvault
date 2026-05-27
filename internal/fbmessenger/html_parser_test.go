package fbmessenger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

func TestParseHTMLThread_Simple(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	root := "testdata/html_simple"
	th, err := ParseHTMLThread(root, threadDir(t, root, "alice_ABC123"))
	require.NoError(err, "parse")
	assert.Len(th.Participants, 2)
	assert.Equal("direct_chat", th.ConvType)
	require.Len(th.Messages, 3)
	// HTML exports do not expose reaction metadata, so the HTML parser
	// must not fabricate a "[reacted: ...]" suffix. Reaction coverage
	// lives in the JSON parser tests + TestImportDYI_ReactionsDualPath.
	wantBodies := []string{
		"Hello",
		"café time?",
		"See you soon",
	}
	for i, w := range wantBodies {
		assert.Equal(w, th.Messages[i].Body, "messages[%d].Body", i)
	}
	assert.Equal("Alice Example", th.Title)
}

func TestParseHTMLThread_WithMedia(t *testing.T) {
	require := requirepkg.New(t)
	root := "testdata/html_with_media"
	th, err := ParseHTMLThread(root, threadDir(t, root, "bob_XYZ789"))
	require.NoError(err, "parse")
	require.Len(th.Messages, 1, "messages: %+v", th.Messages)
	m := th.Messages[0]
	require.Len(m.Attachments, 1)
	_, err = os.Stat(m.Attachments[0].AbsPath)
	assertpkg.NoError(t, err, "attachment should exist on disk")
}

func TestParseHTMLThread_TimestampLayouts(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	want := time.Date(2019, 10, 19, 14, 37, 0, 0, time.UTC)
	for _, name := range []string{"layout1.html", "layout2.html", "layout3.html"} {
		data, err := os.ReadFile(filepath.Join("testdata/html_timestamps", name))
		require.NoError(err)
		// Use parseHTMLLines indirectly through the main parse path by
		// writing into a temp thread dir and calling ParseHTMLThread.
		tmp := t.TempDir()
		threadPath := filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox", "ts_TEST")
		require.NoError(os.MkdirAll(threadPath, 0755))
		require.NoError(os.WriteFile(filepath.Join(threadPath, "message_1.html"), data, 0644))
		th, err := ParseHTMLThread(tmp, threadPath)
		require.NoError(err, "%s: parse", name)
		require.Len(th.Messages, 1, "%s", name)
		assert.True(th.Messages[0].SentAt.Equal(want), "%s: SentAt=%v want %v", name, th.Messages[0].SentAt, want)
		assert.Equal(time.UTC, th.Messages[0].SentAt.Location(), "%s", name)
	}
}

// TestParseHTMLThread_ImagePositioning verifies that images are attached to
// the message block where they appear in the DOM, not to the first empty or
// attachment-less message.
func TestParseHTMLThread_ImagePositioning(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	root := "testdata/html_multi_media"
	th, err := ParseHTMLThread(root, threadDir(t, root, "carol_IMG456"))
	require.NoError(err, "parse")
	require.Len(th.Messages, 3)
	// Message 0: "Hello Carol" — no image, no attachments.
	assert.Empty(th.Messages[0].Attachments, "messages[0].Attachments (image should NOT land here)")
	// Message 1: "Check out this photo" — the image belongs here.
	assert.Len(th.Messages[1].Attachments, 1, "messages[1].Attachments")
	// Message 2: "Nice picture" — no attachments.
	assert.Empty(th.Messages[2].Attachments, "messages[2].Attachments")
}

func TestParseHTMLThread_StructuralParsing(t *testing.T) {
	require := requirepkg.New(t)
	// Replace known class names with random strings; the parser must
	// still find participants, bodies, and timestamps.
	data, err := os.ReadFile("testdata/html_simple/your_activity_across_facebook/messages/inbox/alice_ABC123/message_1.html")
	require.NoError(err)
	body := string(data)
	for _, cls := range []string{"_a706", "_a70e", "_3b0d", "_a6-g", "_a6-p", "_2ph_", "_a6-h", "_a6-i", "_a72d"} {
		body = strings.ReplaceAll(body, cls, "zzq"+cls[1:])
	}
	tmp := t.TempDir()
	threadPath := filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox", "alice_ABC123")
	require.NoError(os.MkdirAll(threadPath, 0755))
	require.NoError(os.WriteFile(filepath.Join(threadPath, "message_1.html"), []byte(body), 0644))
	th, err := ParseHTMLThread(tmp, threadPath)
	require.NoError(err, "parse")
	require.Len(th.Messages, 3)
	assertpkg.Len(t, th.Participants, 2)
}
