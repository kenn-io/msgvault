package tiktokexport

import (
	"archive/zip"
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportArchive(t *testing.T) {
	for _, format := range []string{"json", "txt", "zip"} {
		t.Run(format, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			path := filepath.Join("testdata", format)
			if format == "zip" {
				path = filepath.Join(t.TempDir(), "export.zip")
				f, err := os.Create(path)
				require.NoError(t, err)
				zw := zip.NewWriter(f)
				w, err := zw.Create("export/user_data_tiktok.json")
				require.NoError(t, err)
				data, err := os.ReadFile("testdata/json/user_data_tiktok.json")
				require.NoError(t, err)
				_, err = w.Write(data)
				require.NoError(t, err)
				require.NoError(t, zw.Close())
				require.NoError(t, f.Close())
			}
			sum, err := Import(context.Background(), st, "my-tiktok", path)
			require.NoError(t, err)
			assert.Equal(t, int64(4), sum.MessagesAdded)
			assert.Equal(t, int64(2), sum.Conversations)
			assert.Equal(t, int64(2), sum.FromMe)
			var processed, added int64
			require.NoError(t, st.DB().QueryRow("SELECT messages_processed,messages_added FROM sync_runs WHERE source_id = ? ORDER BY id DESC LIMIT 1", sum.SourceID).Scan(&processed, &added))
			assert.Equal(t, int64(4), processed)
			assert.Equal(t, int64(4), added)
			if format != "txt" {
				assert.Equal(t, 1, sum.UnknownSections)
			}
			var id int64
			require.NoError(t, st.DB().QueryRow("SELECT id FROM messages WHERE snippet = ?", "A synthetic comment\nhttps://www.tiktok.com/@example/video/9007199254740995").Scan(&id))
			var body string
			require.NoError(t, st.DB().QueryRow("SELECT body_text FROM message_bodies WHERE message_id = ?", id).Scan(&body))
			assert.Contains(t, body, "A synthetic comment")
			var n int
			require.NoError(t, st.DB().QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'café'").Scan(&n))
			assert.Equal(t, 1, n)
			require.NoError(t, st.DB().QueryRow("SELECT COUNT(*) FROM messages WHERE source_is_from_me = 1").Scan(&n))
			assert.Equal(t, 2, n)
			source, err := st.GetSourceByTypeAndIdentifier(SourceType, "my-tiktok")
			require.NoError(t, err)
			identities, err := st.ListAccountIdentities(source.ID)
			require.NoError(t, err)
			require.Len(t, identities, 1)
			sum, err = Import(context.Background(), st, "my-tiktok", path)
			require.NoError(t, err)
			assert.Zero(t, sum.MessagesAdded)
			require.NoError(t, st.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&n))
			assert.Equal(t, 4, n)
		})
	}
}

func TestImportRejectsBeforeCreatingSource(t *testing.T) {
	for _, data := range []string{`{`, `{"Direct Messages":{"ChatHistory":{"Chat History with example_friend:":[{"Date":"2026-01-01 00:00:00", "From":"example_friend", "Content":"x"}]}}}`} {
		t.Run(data, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "export.json"), []byte(data), 0600))
			st := testutil.NewTestStore(t)
			_, err := Import(context.Background(), st, "my-tiktok", dir)
			require.Error(t, err)
			var n int
			require.NoError(t, st.DB().QueryRow("SELECT COUNT(*) FROM sources").Scan(&n))
			assert.Zero(t, n)
		})
	}
}

func TestImportRejectsInvalidDateBeforeCreatingSource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	data := `{"Profile":{"Profile Information":{"ProfileMap":{"userName":"archive_owner"}}},"Comments":{"CommentsList":[{"Date":"bad", "Comment":"x"}]}}`
	dir := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(dir, "export.json"), []byte(data), 0600))
	st := testutil.NewTestStore(t)
	_, err := Import(t.Context(), st, "my-tiktok", dir)
	require.ErrorContains(err, "invalid or missing Date; expected YYYY-MM-DD HH:MM:SS or RFC3339")

	var n int
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM sources").Scan(&n))
	assert.Zero(n)
}

func TestReorderedExportAndCrossFormatConverge(t *testing.T) {
	st := testutil.NewTestStore(t)
	first, err := Import(t.Context(), st, "example-account", "testdata/json")
	require.NoError(t, err)
	assert.Equal(t, int64(4), first.MessagesAdded)
	txt, err := Import(t.Context(), st, "example-account", "testdata/txt")
	require.NoError(t, err)
	assert.Zero(t, txt.MessagesAdded)
	data, err := os.ReadFile("testdata/json/user_data_tiktok.json")
	require.NoError(t, err)
	var root map[string]any
	require.NoError(t, json.Unmarshal(data, &root))
	direct, ok := root["Direct Message"].(map[string]any)
	require.True(t, ok)
	wrapper, ok := direct["Direct Messages"].(map[string]any)
	require.True(t, ok)
	chats, ok := wrapper["ChatHistory"].(map[string]any)
	require.True(t, ok)
	records, ok := chats["Chat History with example_friend:"].([]any)
	require.True(t, ok)
	slices.Reverse(records)
	// Reordering and duplicate records must preserve the same archive rows.
	chats["Chat History with example_friend:"] = append(records, records[0])
	data, err = json.Marshal(root)
	require.NoError(t, err)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "renamed.json"), data, 0600))
	reordered, err := Import(t.Context(), st, "example-account", dir)
	require.NoError(t, err)
	assert.Zero(t, reordered.MessagesAdded)
	var n int
	require.NoError(t, st.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&n))
	assert.Equal(t, 4, n)
	// The same source label must not quietly acquire a different owner.
	changed := strings.ReplaceAll(string(data), "archive_owner", "other_owner")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "renamed.json"), []byte(changed), 0600))
	_, err = Import(t.Context(), st, "example-account", dir)
	require.ErrorContains(t, err, "different profile")
}

func TestNativeSelfAttributionSurvivesParticipantMerge(t *testing.T) {
	st := testutil.NewTestStore(t)
	_, err := Import(t.Context(), st, "example-account", "testdata/json")
	require.NoError(t, err)
	var self, partner int64
	require.NoError(t, st.DB().QueryRow("SELECT sender_id FROM messages WHERE source_is_from_me=1 LIMIT 1").Scan(&self))
	require.NoError(t, st.DB().QueryRow("SELECT sender_id FROM messages WHERE source_is_from_me=0 LIMIT 1").Scan(&partner))
	require.NoError(t, st.MergeParticipants(partner, self))
	_, err = Import(t.Context(), st, "example-account", "testdata/json")
	require.NoError(t, err)
	var n int
	require.NoError(t, st.DB().QueryRow("SELECT COUNT(*) FROM messages WHERE source_is_from_me=1").Scan(&n))
	assert.Equal(t, 2, n)
}
