package fbmessenger

import (
	"os"
	"path/filepath"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

func TestParseE2EEJSONFile_Simple(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	root := "testdata/e2ee_simple"
	absRoot, err := filepath.Abs(root)
	require.NoError(err)
	filePath := filepath.Join(absRoot, "your_activity_across_facebook", "messages", "alice_1.json")

	th, err := ParseE2EEJSONFile(absRoot, filePath)
	require.NoError(err, "parse")
	assert.Equal("direct_chat", th.ConvType)
	assert.Len(th.Participants, 2)
	assert.Equal("e2ee_json", th.Format)
	assert.Equal("alice_1", th.DirName)
	// Unsent message should be filtered out.
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
	assert.Len(th.Messages[1].Reactions, 1)
	// Index monotonic.
	for i, m := range th.Messages {
		assert.Equal(i, m.Index, "index[%d]", i)
	}
}

func TestParseE2EEJSONFile_Group(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	root := "testdata/e2ee_simple"
	absRoot, err := filepath.Abs(root)
	require.NoError(err)
	filePath := filepath.Join(absRoot, "your_activity_across_facebook", "messages", "group_2.json")

	th, err := ParseE2EEJSONFile(absRoot, filePath)
	require.NoError(err, "parse")
	assert.Equal("group_chat", th.ConvType)
	assert.Len(th.Participants, 3)
}

func TestParseE2EEJSONFile_MediaResolution(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	root := "testdata/e2ee_simple"
	absRoot, err := filepath.Abs(root)
	require.NoError(err)
	filePath := filepath.Join(absRoot, "your_activity_across_facebook", "messages", "group_2.json")

	th, err := ParseE2EEJSONFile(absRoot, filePath)
	require.NoError(err, "parse")
	require.Len(th.Messages, 2)
	// Second message has a media attachment.
	m := th.Messages[1]
	assert.Equal("[media]", m.Body)
	require.Len(m.Attachments, 1)
	att := m.Attachments[0]
	assert.Equal("photo", att.Kind)
	assert.Equal("photo.jpg", att.Filename)
	_, err = os.Stat(att.AbsPath)
	assert.NoError(err, "attachment file should exist on disk")
}

func TestParseE2EEJSONFile_NotAThread(t *testing.T) {
	tmp := t.TempDir()
	cases := map[string]string{
		"array.json":   `[{"any": "list"}]`,
		"scalar.json":  `"a string"`,
		"no_keys.json": `{"setting": true, "version": 2}`,
	}
	for name, body := range cases {
		p := filepath.Join(tmp, name)
		requirepkg.NoError(t, os.WriteFile(p, []byte(body), 0o644))
		_, err := ParseE2EEJSONFile(tmp, p)
		assertpkg.ErrorIs(t, err, ErrNotE2EEThread, "%s", name)
	}
}

// TestParseE2EEJSONFile_PartialObjectCorrupt verifies that an object
// with exactly one of "participants"/"messages" is classified as corrupt
// rather than silently skipped — a partial export with missing
// messages must not vanish silently.
func TestParseE2EEJSONFile_PartialObjectCorrupt(t *testing.T) {
	tmp := t.TempDir()
	cases := map[string]string{
		"only_p.json":   `{"participants": ["A", "B"]}`,
		"only_msg.json": `{"messages": [{"senderName":"A","text":"x","timestamp":1}]}`,
	}
	for name, body := range cases {
		p := filepath.Join(tmp, name)
		requirepkg.NoError(t, os.WriteFile(p, []byte(body), 0o644))
		_, err := ParseE2EEJSONFile(tmp, p)
		assertpkg.ErrorIs(t, err, ErrCorruptJSON, "%s", name)
	}
}

func TestParseE2EEJSONFile_CorruptJSON(t *testing.T) {
	tmp := t.TempDir()
	badFile := filepath.Join(tmp, "bad.json")
	requirepkg.NoError(t, os.WriteFile(badFile, []byte(`{not valid json`), 0644))
	_, err := ParseE2EEJSONFile(tmp, badFile)
	requirepkg.Error(t, err)
	assertpkg.ErrorIs(t, err, ErrCorruptJSON)
}

func TestParseE2EEJSONFile_PathEscapeRejected(t *testing.T) {
	require := requirepkg.New(t)
	tmp := t.TempDir()
	body := `{
		"participants": ["A", "B"],
		"threadName": "test",
		"messages": [{
			"senderName": "A",
			"text": "",
			"timestamp": 1600000000000,
			"type": "Generic",
			"media": [{"uri": "../../etc/passwd"}]
		}]
	}`
	filePath := filepath.Join(tmp, "evil.json")
	require.NoError(os.WriteFile(filePath, []byte(body), 0644))
	th, err := ParseE2EEJSONFile(tmp, filePath)
	require.NoError(err, "parse")
	require.Len(th.Messages, 1)
	// Path escape should be rejected — no attachments resolved.
	assertpkg.Empty(t, th.Messages[0].Attachments, "path escape not rejected")
}

func TestDiscover_E2EEFlat(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	dirs, err := Discover("testdata/e2ee_simple")
	require.NoError(err, "Discover")
	// e2ee_simple has two JSON files at the messages root: alice_1.json
	// and group_2.json. Both should be discovered as e2ee_json threads.
	require.Len(dirs, 2, "discovered: %+v", dirs)
	for _, d := range dirs {
		assert.Equal("e2ee_json", d.Format, "format for %q", d.Name)
		assert.Equal("e2ee_cutover", d.Section, "section for %q", d.Name)
		assert.NotEmpty(d.FilePath, "FilePath should be set for E2EE thread %q", d.Name)
		assert.True(filepath.IsAbs(d.Path), "path not absolute: %q", d.Path)
	}
	// Sorted by name.
	assert.Equal("alice_1", dirs[0].Name)
	assert.Equal("group_2", dirs[1].Name)
}

// TestDiscover_E2EEFlatRejectsNonThreadJSON verifies that a directory
// containing both real thread files and unknown non-thread JSON blobs
// (e.g. a new DYI metadata file Facebook may add) discovers only the
// thread files. Keeping the indexed list stable across runs is required
// for checkpoint-by-thread-index resume.
func TestDiscover_E2EEFlatRejectsNonThreadJSON(t *testing.T) {
	require := requirepkg.New(t)
	tmp := t.TempDir()
	thread := `{"participants":["A","B"],"threadName":"t","messages":[]}`
	require.NoError(os.WriteFile(filepath.Join(tmp, "real_1.json"), []byte(thread), 0o644))
	require.NoError(os.WriteFile(filepath.Join(tmp, "metadata.json"), []byte(`{"setting":true,"version":3}`), 0o644))
	require.NoError(os.WriteFile(filepath.Join(tmp, "list.json"), []byte(`[1,2,3]`), 0o644))

	dirs, err := Discover(tmp)
	require.NoError(err, "Discover")
	require.Len(dirs, 1, "discovered: %+v", dirs)
	assertpkg.Equal(t, "real_1", dirs[0].Name)
}
