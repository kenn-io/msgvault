package groupme

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/testutil"
)

func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, folder := range []string{"group", "direct", "empty"} {
		require.NoError(t, os.Mkdir(filepath.Join(root, folder), 0700))
		for _, name := range []string{"message.json", "conversation.json"} {
			b, err := os.ReadFile(filepath.Join("testdata", folder, name))
			if os.IsNotExist(err) {
				continue
			}
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(root, folder, name), b, 0600))
		}
	}
	return root
}

func TestImportCoverageRawUnknownsAndReruns(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := fixture(t)
	opts := ImportOptions{RootDir: root, Me: "fictional-owner", AttachmentsDir: t.TempDir()}
	summary, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.EqualValues(t, 4, summary.MessagesAdded)
	assert.EqualValues(t, 2, summary.ServiceMessagesSkipped)
	assert.EqualValues(t, 1, summary.UnsupportedEntriesSkipped)
	assert.Equal(t, 3, summary.ChatsProcessed)
	assert.Equal(t, map[int]int64{2024: 2, 2025: 3}, summary.Years)
	assert.Equal(t, map[string]int64{"message": 5, "system": 2}, summary.Types)
	assert.Equal(t, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), summary.Oldest)
	assert.Equal(t, 2, summary.AttachmentsMissing)
	assert.Equal(t, 1, summary.AttachmentsUnsupported)
	var directTitle, directType string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT title,conversation_type FROM conversations WHERE source_conversation_id='dm:fictional-direct'").Scan(&directTitle, &directType))
	assert.Equal(t, "Example Direct Peer", directTitle)
	assert.Equal(t, "direct_chat", directType)
	key := messageKey("group:fictional-garden", "fictional-one")
	ids, err := st.MessageExistsBatchContext(t.Context(), summary.SourceID, []string{key})
	require.NoError(t, err)
	require.Contains(t, ids, key)
	raw, err := st.GetMessageRawContext(t.Context(), ids[key])
	require.NoError(t, err)
	var m Message
	require.NoError(t, json.Unmarshal(raw, &m))
	assert.Equal(t, "Seedneedle plans for spring", m.Text)
	var parent string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT p.source_message_id FROM messages m JOIN messages p ON p.id=m.reply_to_message_id WHERE m.source_message_id=?"), messageKey("group:fictional-garden", "fictional-two")).Scan(&parent))
	assert.Equal(t, key, parent)
	var unknowns int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages WHERE sender_id IS NULL AND sent_at IS NULL").Scan(&unknowns))
	assert.Equal(t, 1, unknowns)
	var fromMe int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages WHERE is_from_me = true").Scan(&fromMe))
	assert.Equal(t, 1, fromMe)
	_, total, err := st.SearchMessages("Seedneedle", 0, 10)
	require.NoError(t, err)
	assert.EqualValues(t, 1, total)
	require.NoError(t, os.Rename(filepath.Join(root, "group"), filepath.Join(root, "renamed opaque folder")))
	summary, err = Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.Zero(t, summary.MessagesAdded)
	assert.EqualValues(t, 4, summary.MessagesSkipped)
	// A later export cannot replace first-imported text.
	path := filepath.Join(root, "renamed opaque folder", "message.json")
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte(strings.ReplaceAll(string(content), "Seedneedle plans for spring", "replacement")), 0600))
	_, err = Import(t.Context(), st, opts)
	require.NoError(t, err)
	body, _, err := st.GetMessageBodyAndRawContext(t.Context(), ids[key])
	require.NoError(t, err)
	assert.Equal(t, "Seedneedle plans for spring", body)
	// A separate owner has a separate source even with overlapping message IDs.
	opts.Me = "fictional-other-owner"
	second, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.NotEqual(t, summary.SourceID, second.SourceID)
	assert.EqualValues(t, 4, second.MessagesAdded)
}

func TestImportMediaRecovery(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := fixture(t)
	opts := ImportOptions{RootDir: root, Me: "fictional-owner", AttachmentsDir: t.TempDir()}
	first, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.Equal(t, 2, first.AttachmentsMissing)
	// A later export reorders attachments; missing bytes still fill the same slot.
	path := filepath.Join(root, "group", "message.json")
	original, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte(strings.Replace(string(original), `"attachments":[{"type":"image"`, `"attachments":[{"type":"reply","reply_id":"fictional-two"},{"type":"image"`, 1)), 0600))
	gallery := filepath.Join(root, "group", "gallery")
	require.NoError(t, os.Mkdir(gallery, 0700))
	for name, content := range map[string]string{"fictional-one_800x600.fictional-image.png": "fictional image bytes", "fictional-two_fictional-video.720p.mp4": "fictional video bytes"} {
		require.NoError(t, os.WriteFile(filepath.Join(gallery, name), []byte(content), 0600))
	}
	second, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.Equal(t, 2, second.AttachmentsStored)
	assert.Zero(t, second.MessagesAdded)
	ids, err := st.MessageExistsBatchContext(t.Context(), first.SourceID, []string{messageKey("group:fictional-garden", "fictional-one")})
	require.NoError(t, err)
	refs, err := st.MessageMIMEAttachmentsContext(t.Context(), ids[messageKey("group:fictional-garden", "fictional-one")])
	require.NoError(t, err)
	require.Len(t, refs, 1)
	data, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, refs[0].StoragePath))
	require.NoError(t, err)
	assert.Equal(t, "fictional image bytes", string(data))
	require.NoError(t, os.WriteFile(filepath.Join(gallery, "fictional-one_800x600.fictional-image.png"), []byte("changed bytes"), 0600))
	third, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.Zero(t, third.AttachmentsStored)
	data, err = os.ReadFile(filepath.Join(opts.AttachmentsDir, refs[0].StoragePath))
	require.NoError(t, err)
	assert.Equal(t, "fictional image bytes", string(data))
}

type cancelLog struct{ cancel context.CancelFunc }

func (w cancelLog) Write(p []byte) (int, error) { w.cancel(); return len(p), nil }
func TestImportCancellationThenRepeat(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	root := fixture(t)
	summary, err := Import(ctx, st, ImportOptions{RootDir: root, Me: "fictional-owner", Logger: slog.New(slog.NewTextHandler(cancelLog{cancel}, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	require.ErrorIs(t, err, context.Canceled)
	assert.EqualValues(t, 1, summary.MessagesAdded)
	summary, err = Import(t.Context(), st, ImportOptions{RootDir: root, Me: "fictional-owner"})
	require.NoError(t, err)
	assert.EqualValues(t, 3, summary.MessagesAdded)
	assert.EqualValues(t, 1, summary.MessagesSkipped)
	var failed int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM sync_runs WHERE status='failed'").Scan(&failed))
	assert.Equal(t, 1, failed)
}

func TestImportOverlappingReplies(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	path := filepath.Join(root, "message.json")
	require.NoError(t, os.WriteFile(path, []byte(`[{"id":"child","group_id":"example","text":"child","attachments":[{"type":"reply","reply_id":"parent"}]}]`), 0600))
	opts := ImportOptions{RootDir: root, Me: "owner"}
	first, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.EqualValues(t, 1, first.MessagesAdded)
	require.NoError(t, os.WriteFile(path, []byte(`[{"id":"parent","group_id":"example","text":"parent"}]`), 0600))
	_, err = Import(t.Context(), st, opts)
	require.NoError(t, err)
	var parent sql.NullInt64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT reply_to_message_id FROM messages WHERE source_message_id=?"), messageKey("group:example", "child")).Scan(&parent))
	assert.True(t, parent.Valid)
}

func TestReaderRejectsMalformedAndOversized(t *testing.T) {
	for _, tc := range []struct{ name, content, want string }{
		{"object", `{}`, "array"},
		{"truncated", `[{"id":"a"}`, "read message"},
		{"trailing", `[] {}`, "trailing"},
		{"conflicting", `[{"group_id":"a"},{"group_id":"b"}]`, "conflicting"},
		{"oversized", `[{"text":"` + strings.Repeat("x", maxRecordBytes) + `"}]`, "exceeds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(root, "message.json"), []byte(tc.content), 0600))
			r, err := os.OpenRoot(root)
			require.NoError(t, err)
			defer r.Close()
			err = discover(t.Context(), r, root, func(chat) error { return nil })
			require.ErrorContains(t, err, tc.want)
		})
	}
	t.Run("metadata", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(root, "message.json"), []byte(`[]`), 0600))
		require.NoError(t, os.WriteFile(filepath.Join(root, "conversation.json"), []byte(`{"name":"`+strings.Repeat("x", maxMetadataBytes)+`"}`), 0600))
		r, err := os.OpenRoot(root)
		require.NoError(t, err)
		defer r.Close()
		err = discover(t.Context(), r, root, func(chat) error { return nil })
		require.ErrorContains(t, err, "exceeds 1 MiB")
	})
}

func TestReaderAndMediaContainment(t *testing.T) {
	t.Run("message symlink", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(outside, "message.json"), []byte(`[]`), 0600))
		require.NoError(t, os.Symlink(filepath.Join(outside, "message.json"), filepath.Join(root, "message.json")))
		r, err := os.OpenRoot(root)
		require.NoError(t, err)
		defer r.Close()
		err = discover(t.Context(), r, root, func(chat) error { return nil })
		require.Error(t, err)
	})
	t.Run("media symlink", func(t *testing.T) {
		st := testutil.NewTestStore(t)
		root := fixture(t)
		outside := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(outside, "fictional-one_800x600.fictional-image.png"), []byte("outside bytes"), 0600))
		require.NoError(t, os.Symlink(outside, filepath.Join(root, "group", "gallery")))
		summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "owner", AttachmentsDir: t.TempDir()})
		require.NoError(t, err)
		assert.Zero(t, summary.AttachmentsStored)
		assert.Equal(t, 2, summary.AttachmentsMissing)
	})
	for _, id := range []string{"../escape", "/absolute", `..\escape`} {
		name, ok := galleryName(id, Attachment{Type: "image", URL: "https://images.example.invalid/800x600.png.example"})
		assert.False(t, ok)
		assert.Empty(t, name)
	}
}

func TestReaderBoundedManySmallRecords(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "message.json")
	// Exceed a single-record budget overall, with each record safely below it.
	content := `[{"id":"first","text":"` + strings.Repeat("x", maxRecordBytes/2) + `"},{"id":"second","text":"` + strings.Repeat("y", maxRecordBytes/2) + `"}]`
	require.NoError(t, os.WriteFile(path, []byte(content), 0600))
	r, err := os.OpenRoot(root)
	require.NoError(t, err)
	defer r.Close()
	count := 0
	require.NoError(t, messages(t.Context(), r, ".", func(_ *Message, _ jsontext.Value) error { count++; return nil }))
	assert.Equal(t, 2, count)
}

func TestImportRequiresOwner(t *testing.T) {
	st := testutil.NewTestStore(t)
	_, err := Import(t.Context(), st, ImportOptions{RootDir: t.TempDir()})
	require.ErrorContains(t, err, "--me")
}

func TestImportUnknownAttachmentShapesKeepText(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "message.json"), []byte(`[{"id":"one","group_id":"fictional","sender_type":"bot","text":"ordinary needle","attachments":{"future":true}},{"id":"two","group_id":"fictional","text":"ordinary second","attachments":["future",{"type":"image","url":55}]},{"id":"service","group_id":"fictional","sender_type":"service","system":false,"text":"fictional service"}]`), 0600))
	summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "owner"})
	require.NoError(t, err)
	assert.EqualValues(t, 2, summary.MessagesAdded)
	assert.Equal(t, 3, summary.AttachmentsUnsupported)
	assert.EqualValues(t, 1, summary.ServiceMessagesSkipped)
	assert.Equal(t, map[string]int64{"bot": 1, "message": 1, "service": 1}, summary.Types)
	_, total, err := st.SearchMessages("ordinary", 0, 10)
	require.NoError(t, err)
	assert.EqualValues(t, 2, total)
}

func TestImportMediaCapKeepsRetryableSlot(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := fixture(t)
	gallery := filepath.Join(root, "group", "gallery")
	require.NoError(t, os.Mkdir(gallery, 0700))
	path := filepath.Join(gallery, "fictional-one_800x600.fictional-image.png")
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(attachmentpolicy.DefaultChatMaxBytes+1))
	require.NoError(t, f.Close())
	opts := ImportOptions{RootDir: root, Me: "owner", AttachmentsDir: t.TempDir()}
	summary, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.Equal(t, 2, summary.AttachmentsMissing)
	assert.Zero(t, summary.AttachmentsStored)
	require.NoError(t, os.WriteFile(path, []byte("small fictional bytes"), 0600))
	summary, err = Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.AttachmentsStored)
}

func TestImportMalformedLaterConversationKeepsEarlierMessages(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	for _, folder := range []string{"a", "b"} {
		require.NoError(t, os.Mkdir(filepath.Join(root, folder), 0700))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "a", "message.json"), []byte(`[{"id":"first","group_id":"example","text":"first"}]`), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "b", "message.json"), []byte(`[{`), 0600))
	summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "owner"})
	require.Error(t, err)
	assert.EqualValues(t, 1, summary.MessagesAdded)
	assert.Equal(t, 1, summary.ChatsProcessed)
	var n int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages").Scan(&n))
	assert.Equal(t, 1, n)
	require.NoError(t, os.WriteFile(filepath.Join(root, "b", "message.json"), []byte(`[{"id":"second","conversation_id":"example-dm","text":"second"}]`), 0600))
	summary, err = Import(t.Context(), st, ImportOptions{RootDir: root, Me: "owner"})
	require.NoError(t, err)
	assert.EqualValues(t, 1, summary.MessagesAdded)
	assert.EqualValues(t, 1, summary.MessagesSkipped)
}
