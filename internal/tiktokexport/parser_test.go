package tiktokexport

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNamedVariantsAndFallback(t *testing.T) {
	dir := t.TempDir()
	data := `{"Profile and Settings":{"Profile Information":{"Username":"archive_owner"}},"Direct Messages":{"Chat History":{"Chat History with example_friend:":[{"Date":"2026-01-02T03:04:05Z","From":"@ARCHIVE_OWNER","Content":"JSON wins"}]}},"Comments":{"Comments": [{"Date":"2026-01-03 04:05:06","Comment content":"hello","Original post landing page link":"https://example.com/post"}]},"Likes": {}}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "data.json"), []byte(data), 0600))
	txt, err := os.ReadFile("testdata/txt/Direct Messages.txt")
	require.NoError(t, err)
	txt = append(txt, []byte("\nChat History with second_friend:\nDate: 2026-01-04 00:00:00\nFrom: second_friend\nContent: fallback\ncontinued line\n")...)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Direct Messages.txt"), txt, 0600))
	out, err := readArchive(context.Background(), dir)
	require.NoError(t, err)
	assert.Equal(t, "archive_owner", out.Username)
	require.Len(t, out.Messages, 3)
	assert.Equal(t, "JSON wins", out.Messages[1].Text)
	assert.Equal(t, "hello\nhttps://example.com/post", out.Messages[0].body())
	assert.Equal(t, "fallback\ncontinued line", out.Messages[2].Text)
	assert.Equal(t, 1, out.UnknownSections)
}

func TestEmptyJSONCommentsAllowTXTFallback(t *testing.T) {
	dir := t.TempDir()
	data := `{"Profile":{"Profile Information":{"Username":"archive_owner"}},"Comments":{"CommentsList":[ ]}}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "data.json"), []byte(data), 0600))
	comments, err := os.ReadFile("testdata/txt/Comments.txt")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Comments.txt"), comments, 0600))
	out, err := readArchive(t.Context(), dir)
	require.NoError(t, err)
	require.Len(t, out.Messages, 1)
	assert.Equal(t, "A synthetic comment", out.Messages[0].Text)
}

func TestMessageKeysIgnoreOrderingAndKeepChatAndContent(t *testing.T) {
	a, err := readArchive(context.Background(), "testdata/json")
	require.NoError(t, err)
	m := a.Messages[1]
	// Raw TXT chat headers must not collapse keys when their bytes are not UTF-8.
	invalid := m
	invalid.Chat = "\xff"
	another := invalid
	another.Text += "!"
	assert.NotEqual(t, invalid.key(), another.key())
	changed := m
	changed.Chat = "another_friend"
	assert.NotEqual(t, m.key(), changed.key())
	changed = m
	changed.Text += "!"
	assert.NotEqual(t, m.key(), changed.key())
	changed = m
	changed.Sender = "another_friend"
	assert.NotEqual(t, m.key(), changed.key())
	b, err := readArchive(context.Background(), "testdata/txt")
	require.NoError(t, err)
	require.Len(t, b.Messages, len(a.Messages))
	for i := range a.Messages {
		assert.Equal(t, a.Messages[i].key(), b.Messages[i].key())
	}
}

func FuzzParseJSON(f *testing.F) {
	f.Add("café 🌱", "another message")
	f.Add("Content: a field-looking line\nDate: literal text", "")
	f.Fuzz(func(t *testing.T, first, second string) {
		first = strings.ToValidUTF8(first, "?")
		second = strings.ToValidUTF8(second, "?")
		if strings.TrimSpace(first) == "" || strings.TrimSpace(second) == "" {
			t.Skip()
		}
		records := []map[string]string{
			{"Date": "2026-01-01 00:00:00", "From": "example_owner", "Content": first},
			{"Date": "2026-01-01 00:00:00", "From": "example_friend", "Content": second},
		}
		encode := func() []byte {
			data, err := json.Marshal(map[string]any{"Direct Messages": map[string]any{"ChatHistory": map[string]any{"Chat History with example_friend:": records}}})
			require.NoError(t, err)
			return data
		}
		var a, b archive
		require.NoError(t, a.parseJSON(encode()))
		require.Len(t, a.Messages, 2)
		assert.Equal(t, first, a.Messages[0].Text)
		assert.Equal(t, second, a.Messages[1].Text)
		slices.Reverse(records)
		require.NoError(t, b.parseJSON(encode()))
		require.Len(t, b.Messages, 2)
		assert.Equal(t, a.Messages[0].key(), b.Messages[1].key())
		assert.Equal(t, a.Messages[1].key(), b.Messages[0].key())
	})
}
