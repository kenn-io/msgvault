package fbmessenger

import (
	"os"
	"path/filepath"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

func threadDir(t *testing.T, root, name string) string {
	t.Helper()
	abs, err := filepath.Abs(root)
	requirepkg.NoError(t, err)
	return filepath.Join(abs, "your_activity_across_facebook", "messages", "inbox", name)
}

func TestParseJSONThread_Simple(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	root := "testdata/json_simple"
	th, err := ParseJSONThread(root, threadDir(t, root, "alice_ABC123"))
	require.NoError(err, "parse")
	assert.Equal("direct_chat", th.ConvType)
	assert.Len(th.Participants, 2)
	require.Len(th.Messages, 3)
	// Messages must be chronological ascending.
	for i := 1; i < len(th.Messages); i++ {
		assert.False(th.Messages[i-1].SentAt.After(th.Messages[i].SentAt),
			"messages out of order at %d", i)
	}
	// Mojibake repair: message 1 body must contain "café".
	assert.Contains(th.Messages[1].Body, "café", "mojibake not repaired")
	// Reactions appended to body.
	assert.Contains(th.Messages[1].Body, "[reacted:", "reactions not appended")
	assert.Len(th.Messages[1].Reactions, 2)
	// Index monotonic.
	for i, m := range th.Messages {
		assert.Equal(i, m.Index, "index[%d]", i)
	}
}

func TestParseJSONThread_Group(t *testing.T) {
	root := "testdata/json_group"
	th, err := ParseJSONThread(root, threadDir(t, root, "crew_GRP123"))
	requirepkg.NoError(t, err, "parse")
	assertpkg.Equal(t, "group_chat", th.ConvType)
	assertpkg.Len(t, th.Participants, 3)
}

func TestParseJSONThread_Multifile_NumericSort(t *testing.T) {
	root := "testdata/json_multifile"
	th, err := ParseJSONThread(root, threadDir(t, root, "dave_MULTI"))
	requirepkg.NoError(t, err, "parse")
	requirepkg.Len(t, th.Messages, 4)
	// Bodies, in chronological order, must be A,B,C,D.
	wantBodies := []string{
		"Message A (from file 1, oldest)",
		"Message B (from file 1, newer)",
		"Message C (from file 2)",
		"Message D (from file 10, newest)",
	}
	for i, w := range wantBodies {
		assertpkg.Equal(t, w, th.Messages[i].Body, "messages[%d].Body", i)
	}
}

func TestParseJSONThread_Corrupt(t *testing.T) {
	root := "testdata/corrupt"
	_, err := ParseJSONThread(root, threadDir(t, root, "broken_BAD"))
	requirepkg.Error(t, err)
	assertpkg.ErrorIs(t, err, ErrCorruptJSON)
}

func TestParseJSONThread_Attachments(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	root := "testdata/json_with_media"
	th, err := ParseJSONThread(root, threadDir(t, root, "bob_XYZ789"))
	require.NoError(err, "parse")
	require.Len(th.Messages, 1)
	m := th.Messages[0]
	require.Len(m.Attachments, 1)
	assert.Equal("photo", m.Attachments[0].Kind)
	_, err = os.Stat(m.Attachments[0].AbsPath)
	require.NoError(err, "attachment file should exist on disk")
	assert.Equal("image/png", m.Attachments[0].MimeType)
}

func TestParseJSONThread_Attachments_AltLayout(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	root := "testdata/json_with_media_alt"
	absRoot, err := filepath.Abs(root)
	require.NoError(err)
	td := filepath.Join(absRoot, "your_facebook_activity", "messages", "inbox", "carol_ALT456")
	th, err := ParseJSONThread(root, td)
	require.NoError(err, "parse")
	require.Len(th.Messages, 1)
	m := th.Messages[0]
	require.Len(m.Attachments, 1)
	assert.Equal("photo", m.Attachments[0].Kind)
	_, err = os.Stat(m.Attachments[0].AbsPath)
	require.NoError(err, "attachment file should exist on disk")
}

func TestParseJSONThread_NonTextBodies(t *testing.T) {
	root := "testdata/json_nontext"
	th, err := ParseJSONThread(root, threadDir(t, root, "sam_NONTXT"))
	requirepkg.NoError(t, err, "parse")
	// Ordered chronologically ascending: unsubscribe, share, missed call, call, photo, sticker.
	wantBodies := []string{
		"[system] Sam left the chat",
		"[shared link] https://example.com/article\nExample share text",
		"[call: missed, 0s]",
		"[call: 3m 12s]",
		"[photo]",
		"[sticker]",
	}
	requirepkg.Len(t, th.Messages, len(wantBodies))
	for i, w := range wantBodies {
		assertpkg.Equal(t, w, th.Messages[i].Body, "messages[%d].Body", i)
	}
}

func TestParseJSONThread_PathEscapeRejected(t *testing.T) {
	require := requirepkg.New(t)
	tmp := t.TempDir()
	threadPath := filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox", "evil_ESC")
	require.NoError(os.MkdirAll(threadPath, 0755))
	body := `{"participants":[{"name":"A"},{"name":"B"}],"messages":[
{"sender_name":"A","timestamp_ms":1600000000000,"type":"Generic","photos":[{"uri":"../../etc/passwd"}]}
],"title":"x"}`
	require.NoError(os.WriteFile(filepath.Join(threadPath, "message_1.json"), []byte(body), 0644))
	th, err := ParseJSONThread(tmp, threadPath)
	require.NoError(err, "parse")
	require.Len(th.Messages, 1)
	att := th.Messages[0].Attachments
	require.Len(att, 1)
	assertpkg.Empty(t, att[0].AbsPath, "path escape not rejected")
}

// When a thread dir has no valid numbered message files at all, the
// parser returns an error because there is nothing to import.
func TestParseJSONThread_OnlyUnnumberedFiles(t *testing.T) {
	tmp := t.TempDir()
	threadPath := filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox", "bad_NAME")
	requirepkg.NoError(t, os.MkdirAll(threadPath, 0755))
	requirepkg.NoError(t, os.WriteFile(filepath.Join(threadPath, "message_final.json"), []byte(`{}`), 0644))
	_, err := ParseJSONThread(tmp, threadPath)
	requirepkg.Error(t, err, "expected error when no valid numbered files present")
}

// When a thread dir contains BOTH valid numbered files and a sibling
// whose name doesn't match the `^message_(\d+)\.json$` pattern, the
// parser must import the valid file(s) and report the bad sibling via
// Thread.BadSiblings rather than aborting the entire thread.
func TestParseJSONThread_SkipsUnnumberedSibling(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmp := t.TempDir()
	threadPath := filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox", "mix_MIXED")
	require.NoError(os.MkdirAll(threadPath, 0755))
	good := `{"participants":[{"name":"A"},{"name":"B"}],"messages":[
{"sender_name":"A","timestamp_ms":1600000000000,"type":"Generic","content":"hi from A"}
],"title":"mix"}`
	require.NoError(os.WriteFile(filepath.Join(threadPath, "message_1.json"), []byte(good), 0644))
	// Facebook sometimes writes a human-named sibling; content doesn't
	// matter because we skip it by name before attempting to parse it.
	require.NoError(os.WriteFile(filepath.Join(threadPath, "message_final.json"), []byte(`not even valid json`), 0644))
	th, err := ParseJSONThread(tmp, threadPath)
	require.NoError(err, "unexpected error")
	require.Len(th.Messages, 1)
	assert.Equal("hi from A", th.Messages[0].Body)
	require.Len(th.BadSiblings, 1)
	assert.Equal("message_final.json", th.BadSiblings[0], "BadSiblings=%v", th.BadSiblings)
}
