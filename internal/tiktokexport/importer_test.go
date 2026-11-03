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
			assert := assert.New(t)
			require := require.New(t)

			st := testutil.NewTestStore(t)
			path := filepath.Join("testdata", format)
			if format == "zip" {
				path = filepath.Join(t.TempDir(), "export.zip")
				f, err := os.Create(path)
				require.NoError(err)
				zw := zip.NewWriter(f)
				w, err := zw.Create("export/user_data_tiktok.json")
				require.NoError(err)
				data, err := os.ReadFile("testdata/json/user_data_tiktok.json")
				require.NoError(err)
				_, err = w.Write(data)
				require.NoError(err)
				require.NoError(zw.Close())
				require.NoError(f.Close())
			}
			sum, err := Import(context.Background(), st, "my-tiktok", path)
			require.NoError(err)
			assert.Equal(int64(4), sum.MessagesAdded)
			assert.Equal(int64(2), sum.Conversations)
			assert.Equal(int64(2), sum.FromMe)
			var processed, added int64
			require.NoError(st.DB().QueryRow(st.Rebind("SELECT messages_processed,messages_added FROM sync_runs WHERE source_id = ? ORDER BY id DESC LIMIT 1"), sum.SourceID).Scan(&processed, &added))
			assert.Equal(int64(4), processed)
			assert.Equal(int64(4), added)
			if format != "txt" {
				assert.Equal(1, sum.UnknownSections)
			}
			var id int64
			require.NoError(st.DB().QueryRow(st.Rebind("SELECT id FROM messages WHERE snippet = ?"), "A synthetic comment\nhttps://www.tiktok.com/@example/video/9007199254740995").Scan(&id))
			var body string
			require.NoError(st.DB().QueryRow(st.Rebind("SELECT body_text FROM message_bodies WHERE message_id = ?"), id).Scan(&body))
			assert.Contains(body, "A synthetic comment")
			var n int
			matches, total, err := st.SearchMessages("café", 0, 10)
			require.NoError(err)
			assert.Len(matches, 1)
			assert.Equal(int64(1), total)
			require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM messages WHERE source_is_from_me = TRUE").Scan(&n))
			assert.Equal(2, n)
			source, err := st.GetSourceByTypeAndIdentifier(SourceType, "my-tiktok")
			require.NoError(err)
			identities, err := st.ListAccountIdentities(source.ID)
			require.NoError(err)
			require.Len(identities, 1)
			sum, err = Import(context.Background(), st, "my-tiktok", path)
			require.NoError(err)
			assert.Zero(sum.MessagesAdded)
			require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&n))
			assert.Equal(4, n)
		})
	}
}

func TestImportRejectsBeforeCreatingSource(t *testing.T) {
	for _, data := range []string{`{`, `{"Direct Messages":{"ChatHistory":{"Chat History with example_friend:":[{"Date":"2026-01-01 00:00:00", "From":"example_friend", "Content":"x"}]}}}`} {
		t.Run(data, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			dir := t.TempDir()
			require.NoError(os.WriteFile(filepath.Join(dir, "export.json"), []byte(data), 0600))
			st := testutil.NewTestStore(t)
			_, err := Import(context.Background(), st, "my-tiktok", dir)
			require.Error(err)
			var n int
			require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM sources").Scan(&n))
			assert.Zero(n)
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
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	first, err := Import(t.Context(), st, "example-account", "testdata/json")
	require.NoError(err)
	assert.Equal(int64(4), first.MessagesAdded)
	txt, err := Import(t.Context(), st, "example-account", "testdata/txt")
	require.NoError(err)
	assert.Zero(txt.MessagesAdded)
	data, err := os.ReadFile("testdata/json/user_data_tiktok.json")
	require.NoError(err)
	var root map[string]any
	require.NoError(json.Unmarshal(data, &root))
	direct, ok := root["Direct Message"].(map[string]any)
	require.True(ok)
	wrapper, ok := direct["Direct Messages"].(map[string]any)
	require.True(ok)
	chats, ok := wrapper["ChatHistory"].(map[string]any)
	require.True(ok)
	records, ok := chats["Chat History with example_friend:"].([]any)
	require.True(ok)
	slices.Reverse(records)
	// Reordering and duplicate records must preserve the same archive rows.
	chats["Chat History with example_friend:"] = append(records, records[0])
	data, err = json.Marshal(root)
	require.NoError(err)
	dir := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(dir, "renamed.json"), data, 0600))
	reordered, err := Import(t.Context(), st, "example-account", dir)
	require.NoError(err)
	assert.Zero(reordered.MessagesAdded)
	var n int
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&n))
	assert.Equal(4, n)
	// The same source label must not quietly acquire a different owner.
	changed := strings.ReplaceAll(string(data), "archive_owner", "other_owner")
	require.NoError(os.WriteFile(filepath.Join(dir, "renamed.json"), []byte(changed), 0600))
	_, err = Import(t.Context(), st, "example-account", dir)
	require.ErrorContains(err, "different profile")
}

func TestNativeSelfAttributionSurvivesParticipantMerge(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	_, err := Import(t.Context(), st, "example-account", "testdata/json")
	require.NoError(err)
	var self, partner int64
	require.NoError(st.DB().QueryRow("SELECT sender_id FROM messages WHERE source_is_from_me=TRUE LIMIT 1").Scan(&self))
	require.NoError(st.DB().QueryRow("SELECT sender_id FROM messages WHERE source_is_from_me=FALSE LIMIT 1").Scan(&partner))
	require.NoError(st.MergeParticipants(partner, self))
	_, err = Import(t.Context(), st, "example-account", "testdata/json")
	require.NoError(err)
	var n int
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM messages WHERE source_is_from_me=TRUE").Scan(&n))
	assert.Equal(2, n)
}

func TestTXTEnrichedLinksPersistWithProvenance(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	dir := linkFallbackFixture(t)
	sum, err := Import(t.Context(), st, "example-account", dir)
	require.NoError(err)
	assert.Equal(int64(2), sum.MessagesAdded)
	var id int64
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT id FROM messages WHERE snippet = ?"), "Hello\nhttps://example.com/shared-image.png").Scan(&id))
	metadata, err := st.GetMessageMetadataContext(t.Context(), id)
	require.NoError(err)
	var fields map[string]string
	require.True(metadata.Valid)
	require.NoError(json.Unmarshal([]byte(metadata.String), &fields))
	assert.Equal("txt", fields["link_source"])
	assert.Equal("https://example.com/shared-image.png", fields["post_url"])
	var body string
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT body_text FROM message_bodies WHERE message_id = ?"), id).Scan(&body))
	assert.Equal("Hello\nhttps://example.com/shared-image.png", body)
	sum, err = Import(t.Context(), st, "example-account", dir)
	require.NoError(err)
	assert.Zero(sum.MessagesAdded)
}
