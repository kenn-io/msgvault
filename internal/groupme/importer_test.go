package groupme

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
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
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := fixture(t)
	groupPath := filepath.Join(root, "group", "message.json")
	groupJSON, err := os.ReadFile(groupPath)
	require.NoError(err)
	groupJSON = append([]byte(strings.TrimSuffix(strings.TrimSpace(string(groupJSON)), "]")), []byte(`,{"id":"service","group_id":"fictional-garden","sender_type":"service","system":false,"text":"fictional service"}]`)...)
	require.NoError(os.WriteFile(groupPath, groupJSON, 0600))
	opts := ImportOptions{RootDir: root, Me: "fictional-owner", AttachmentsDir: t.TempDir()}
	summary, err := Import(t.Context(), st, opts)
	require.NoError(err)
	assert.EqualValues(4, summary.MessagesAdded)
	assert.EqualValues(3, summary.ServiceMessagesSkipped)
	assert.EqualValues(1, summary.UnsupportedEntriesSkipped)
	assert.Equal(3, summary.ChatsProcessed)
	assert.Equal(map[int]int64{2024: 2, 2025: 3}, summary.Years)
	assert.Equal(map[string]int64{"message": 5, "system": 2, "service": 1}, summary.Types)
	assert.Equal(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), summary.Oldest)
	assert.Equal(2, summary.AttachmentsMissing)
	assert.Equal(1, summary.AttachmentsUnsupported)
	var directTitle, directType string
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT title,conversation_type FROM conversations WHERE source_conversation_id='dm:fictional-direct'").Scan(&directTitle, &directType))
	assert.Equal("Example Direct Peer", directTitle)
	assert.Equal("direct_chat", directType)
	key := messageKey("group:fictional-garden", "fictional-one")
	ids, err := st.MessageExistsBatchContext(t.Context(), summary.SourceID, []string{key})
	require.NoError(err)
	require.Contains(ids, key)
	raw, err := st.GetMessageRawContext(t.Context(), ids[key])
	require.NoError(err)
	var m Message
	require.NoError(json.Unmarshal(raw, &m))
	assert.Equal("Seedneedle plans for spring", m.Text)
	var unknowns int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages WHERE sender_id IS NULL AND sent_at IS NULL").Scan(&unknowns))
	assert.Equal(1, unknowns)
	var fromMe int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages WHERE is_from_me = true").Scan(&fromMe))
	assert.Equal(1, fromMe)
	_, total, err := st.SearchMessages("Seedneedle", 0, 10)
	require.NoError(err)
	assert.EqualValues(1, total)
	require.NoError(os.Rename(filepath.Join(root, "group"), filepath.Join(root, "renamed opaque folder")))
	summary, err = Import(t.Context(), st, opts)
	require.NoError(err)
	assert.Zero(summary.MessagesAdded)
	assert.EqualValues(4, summary.MessagesSkipped)
	// A later export cannot replace first-imported text.
	path := filepath.Join(root, "renamed opaque folder", "message.json")
	content, err := os.ReadFile(path)
	require.NoError(err)
	require.NoError(os.WriteFile(path, []byte(strings.ReplaceAll(string(content), "Seedneedle plans for spring", "replacement")), 0600))
	_, err = Import(t.Context(), st, opts)
	require.NoError(err)
	body, _, err := st.GetMessageBodyAndRawContext(t.Context(), ids[key])
	require.NoError(err)
	assert.Equal("Seedneedle plans for spring", body)
	// A separate owner has a separate source even with overlapping message IDs.
	opts.Me = "fictional-other-owner"
	second, err := Import(t.Context(), st, opts)
	require.NoError(err)
	assert.NotEqual(summary.SourceID, second.SourceID)
	assert.EqualValues(4, second.MessagesAdded)
}

func TestImportMedia(t *testing.T) {
	t.Run("recover missing bytes into the same slot", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		st := testutil.NewTestStore(t)
		root := fixture(t)
		opts := ImportOptions{RootDir: root, Me: "fictional-owner", AttachmentsDir: t.TempDir()}
		first, err := Import(t.Context(), st, opts)
		require.NoError(err)
		// A later export reorders attachments; missing bytes still fill the same slot.
		path := filepath.Join(root, "group", "message.json")
		original, err := os.ReadFile(path)
		require.NoError(err)
		require.NoError(os.WriteFile(path, []byte(strings.Replace(string(original), `"attachments":[{"type":"image"`, `"attachments":[{"type":"reply","reply_id":"fictional-two"},{"type":"image"`, 1)), 0600))
		gallery := filepath.Join(root, "group", "gallery")
		require.NoError(os.Mkdir(gallery, 0700))
		for name, content := range map[string]string{"fictional-one_800x600.fictional-image.png": "fictional image bytes", "fictional-two_fictional-video.720p.mp4": "fictional video bytes"} {
			require.NoError(os.WriteFile(filepath.Join(gallery, name), []byte(content), 0600))
		}
		second, err := Import(t.Context(), st, opts)
		require.NoError(err)
		assert.Equal(2, second.AttachmentsStored)
		assert.Zero(second.MessagesAdded)
		ids, err := st.MessageExistsBatchContext(t.Context(), first.SourceID, []string{messageKey("group:fictional-garden", "fictional-one")})
		require.NoError(err)
		refs, err := st.MessageMIMEAttachmentsContext(t.Context(), ids[messageKey("group:fictional-garden", "fictional-one")])
		require.NoError(err)
		require.Len(refs, 1)
		data, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, refs[0].StoragePath))
		require.NoError(err)
		assert.Equal("fictional image bytes", string(data))
		require.NoError(os.WriteFile(filepath.Join(gallery, "fictional-one_800x600.fictional-image.png"), []byte("changed bytes"), 0600))
		third, err := Import(t.Context(), st, opts)
		require.NoError(err)
		assert.Zero(third.AttachmentsStored)
	})
	t.Run("oversized media stays retryable", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		st := testutil.NewTestStore(t)
		root := fixture(t)
		gallery := filepath.Join(root, "group", "gallery")
		require.NoError(os.Mkdir(gallery, 0700))
		path := filepath.Join(gallery, "fictional-one_800x600.fictional-image.png")
		f, err := os.Create(path)
		require.NoError(err)
		require.NoError(f.Truncate(attachmentpolicy.DefaultChatMaxBytes + 1))
		require.NoError(f.Close())
		opts := ImportOptions{RootDir: root, Me: "owner", AttachmentsDir: t.TempDir()}
		summary, err := Import(t.Context(), st, opts)
		require.NoError(err)
		assert.Equal(1, summary.AttachmentsMissing)
		assert.Equal(1, summary.AttachmentsOversized)
		assert.Zero(summary.AttachmentsStored)
		require.NoError(os.WriteFile(path, []byte("small fictional bytes"), 0600))
		summary, err = Import(t.Context(), st, opts)
		require.NoError(err)
		assert.Equal(1, summary.AttachmentsStored)
	})
	t.Run("media symlink", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		st := testutil.NewTestStore(t)
		root := fixture(t)
		outside := t.TempDir()
		require.NoError(os.WriteFile(filepath.Join(outside, "fictional-one_800x600.fictional-image.png"), []byte("outside bytes"), 0600))
		require.NoError(os.Symlink(outside, filepath.Join(root, "group", "gallery")))
		summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "owner", AttachmentsDir: t.TempDir()})
		require.NoError(err)
		assert.Zero(summary.AttachmentsStored)
		assert.Equal(2, summary.AttachmentsMissing)
	})
	for _, id := range []string{"../escape", "/absolute", `..\escape`} {
		name, ok := galleryName(id, Attachment{Type: "image", URL: "https://images.example.invalid/800x600.png.example"})
		assert.False(t, ok)
		assert.Empty(t, name)
	}
}

type cancelLog struct{ cancel context.CancelFunc }

func (w cancelLog) Write(p []byte) (int, error) { w.cancel(); return len(p), nil }
func TestImportCancellationThenRepeat(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	root := fixture(t)
	summary, err := Import(ctx, st, ImportOptions{RootDir: root, Me: "fictional-owner", Logger: slog.New(slog.NewTextHandler(cancelLog{cancel}, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	require.ErrorIs(err, context.Canceled)
	assert.EqualValues(1, summary.MessagesAdded)
	summary, err = Import(t.Context(), st, ImportOptions{RootDir: root, Me: "fictional-owner"})
	require.NoError(err)
	assert.EqualValues(3, summary.MessagesAdded)
	assert.EqualValues(1, summary.MessagesSkipped)
	var failed int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM sync_runs WHERE status='failed'").Scan(&failed))
	assert.Equal(1, failed)
}

func TestImportOverlappingReplies(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	path := filepath.Join(root, "message.json")
	require.NoError(os.WriteFile(path, []byte(`[{"id":"child","group_id":"example","text":"child","attachments":[{"type":"reply","reply_id":"parent"}]}]`), 0600))
	opts := ImportOptions{RootDir: root, Me: "owner"}
	first, err := Import(t.Context(), st, opts)
	require.NoError(err)
	assert.EqualValues(1, first.MessagesAdded)
	require.NoError(os.WriteFile(path, []byte(`[{"id":"parent","group_id":"example","text":"parent"}]`), 0600))
	_, err = Import(t.Context(), st, opts)
	require.NoError(err)
	var parent sql.NullInt64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT reply_to_message_id FROM messages WHERE source_message_id=?"), messageKey("group:example", "child")).Scan(&parent))
	assert.True(parent.Valid)
}

func TestReaderRecordLimitsAndMalformedInput(t *testing.T) {
	for _, tc := range []struct{ name, content, want string }{
		{"multiple records within individual budgets", `[{"id":"first","text":"` + strings.Repeat("x", maxRecordBytes/2) + `"},{"id":"second","text":"` + strings.Repeat("y", maxRecordBytes/2) + `"}]`, ""},
		{"object", `{}`, "array"},
		{"truncated", `[{"id":"a"}`, "read message"},
		{"trailing", `[] {}`, "trailing"},
		{"conflicting", `[{"group_id":"a"},{"group_id":"b"}]`, "conflicting"},
		{"oversized", `[{"text":"` + strings.Repeat("x", maxRecordBytes) + `"}]`, "exceeds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			root := t.TempDir()
			require.NoError(os.WriteFile(filepath.Join(root, "message.json"), []byte(tc.content), 0600))
			r, err := os.OpenRoot(root)
			require.NoError(err)
			defer func() { require.NoError(r.Close()) }()
			err = discover(t.Context(), r, root, func(chat) error { return nil })
			if tc.want == "" {
				require.NoError(err)
			} else {
				require.ErrorContains(err, tc.want)
			}
		})
	}
	t.Run("metadata", func(t *testing.T) {
		require := require.New(t)
		root := t.TempDir()
		require.NoError(os.WriteFile(filepath.Join(root, "message.json"), []byte(`[]`), 0600))
		require.NoError(os.WriteFile(filepath.Join(root, "conversation.json"), []byte(`{"name":"`+strings.Repeat("x", maxMetadataBytes)+`"}`), 0600))
		r, err := os.OpenRoot(root)
		require.NoError(err)
		defer func() { require.NoError(r.Close()) }()
		err = discover(t.Context(), r, root, func(chat) error { return nil })
		require.ErrorContains(err, "exceeds 1 MiB")
	})
}

func TestReaderContainment(t *testing.T) {
	t.Run("message symlink", func(t *testing.T) {
		require := require.New(t)
		root := t.TempDir()
		outside := t.TempDir()
		require.NoError(os.WriteFile(filepath.Join(outside, "message.json"), []byte(`[]`), 0600))
		require.NoError(os.Symlink(filepath.Join(outside, "message.json"), filepath.Join(root, "message.json")))
		r, err := os.OpenRoot(root)
		require.NoError(err)
		defer func() { require.NoError(r.Close()) }()
		err = discover(t.Context(), r, root, func(chat) error { return nil })
		require.ErrorContains(err, "path escapes from parent")
	})
}

func TestImportRequiresOwner(t *testing.T) {
	st := testutil.NewTestStore(t)
	_, err := Import(t.Context(), st, ImportOptions{RootDir: t.TempDir()})
	require.ErrorContains(t, err, "--me")
}

func TestImportUnknownAttachmentShapesKeepText(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(root, "message.json"), []byte(`[{"id":"one","group_id":"fictional","sender_type":"bot","text":"ordinary needle","attachments":{"future":true}},{"id":"two","group_id":"fictional","text":"ordinary second","attachments":["future",{"type":"image","url":55}]}]`), 0600))
	summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "owner"})
	require.NoError(err)
	assert.EqualValues(2, summary.MessagesAdded)
	assert.Equal(3, summary.AttachmentsUnsupported)
	_, total, err := st.SearchMessages("ordinary", 0, 10)
	require.NoError(err)
	assert.EqualValues(2, total)
}

func TestImportMalformedLaterConversationKeepsEarlierMessages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	for _, folder := range []string{"a", "b"} {
		require.NoError(os.Mkdir(filepath.Join(root, folder), 0700))
	}
	require.NoError(os.WriteFile(filepath.Join(root, "a", "message.json"), []byte(`[{"id":"first","group_id":"example","text":"first"}]`), 0600))
	require.NoError(os.WriteFile(filepath.Join(root, "b", "message.json"), []byte(`[{`), 0600))
	summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "owner"})
	require.Error(err)
	assert.EqualValues(1, summary.MessagesAdded)
	assert.Equal(1, summary.ChatsProcessed)
	var n int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages").Scan(&n))
	assert.Equal(1, n)
	require.NoError(os.WriteFile(filepath.Join(root, "b", "message.json"), []byte(`[{"id":"second","conversation_id":"example-dm","text":"second"}]`), 0600))
	summary, err = Import(t.Context(), st, ImportOptions{RootDir: root, Me: "owner"})
	require.NoError(err)
	assert.EqualValues(1, summary.MessagesAdded)
	assert.EqualValues(1, summary.MessagesSkipped)
}

func TestImportMetadataIdentityAndFolderScope(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	group := filepath.Join(root, "original")
	require.NoError(os.Mkdir(group, 0700))
	require.NoError(os.WriteFile(filepath.Join(group, "message.json"), []byte(`[]`), 0600))
	require.NoError(os.WriteFile(filepath.Join(group, "conversation.json"), []byte(`{"id":"opaque-group","members":[{"user_id":"metadata-only","name":"Example Member"}]}`), 0600))
	_, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "owner"})
	require.NoError(err)
	renamed := filepath.Join(root, "renamed")
	require.NoError(os.Rename(group, renamed))
	_, err = Import(t.Context(), st, ImportOptions{RootDir: renamed, Me: "owner"})
	require.NoError(err)
	var n int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM conversations").Scan(&n))
	assert.Equal(1, n)
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_participants").Scan(&n))
	assert.Equal(1, n)
	require.NoError(os.WriteFile(filepath.Join(renamed, "message.json"), []byte(`[{"id":"one","group_id":"different","text":"must not merge"}]`), 0600))
	_, err = Import(t.Context(), st, ImportOptions{RootDir: root, Me: "owner"})
	require.ErrorContains(err, "conflicting conversation IDs")
	// A metadata-free conversation keeps the same fallback across parent/single
	// selections and extraction roots, using its exported folder name.
	for i := range 2 {
		moved := filepath.Join(t.TempDir(), "exported-folder")
		require.NoError(os.Mkdir(moved, 0700))
		require.NoError(os.WriteFile(filepath.Join(moved, "message.json"), []byte(`[{"id":"fallback","text":"folder message"}]`), 0600))
		summary, err := Import(t.Context(), st, ImportOptions{RootDir: filepath.Dir(moved), Me: "owner"})
		require.NoError(err)
		if i == 0 {
			assert.EqualValues(1, summary.MessagesAdded)
		} else {
			assert.Zero(summary.MessagesAdded)
		}
		summary, err = Import(t.Context(), st, ImportOptions{RootDir: moved, Me: "owner"})
		require.NoError(err)
		assert.Zero(summary.MessagesAdded)
	}
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages").Scan(&n))
	assert.Equal(1, n)
}

func TestImportSkipsScalarDrift(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(root, "message.json"), []byte(`[
 {"id":"before","group_id":"opaque","created_at":1704067200,"sender_id":"owner","text":"first"},
 {"id":42,"group_id":"opaque","text":"unsupported ID"},
 {"id":"bad-time","created_at":"yesterday"},
 {"id":"after","group_id":"opaque","created_at":1704067201,"text":"last"} ]`), 0600))
	summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "  owner  "})
	require.NoError(err)
	assert.EqualValues(2, summary.MessagesAdded)
	assert.EqualValues(2, summary.UnsupportedEntriesSkipped)
	assert.Equal(map[int]int64{2024: 2}, summary.Years)
	assert.Equal(map[string]int64{"message": 2}, summary.Types)
	var n int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages WHERE is_from_me=true").Scan(&n))
	assert.Equal(1, n)
}

func TestImportPreservesRawAndDuplicates(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	var records []string
	for i := range maxMessageBatch + 1 {
		records = append(records, fmt.Sprintf(`{"id":"message-%d","group_id":"batch","text":"body-%d"}`, i, i))
		if i == 0 {
			records = append(records, `{"id":"message-0","group_id":"batch","text":"must not overwrite"}`)
		}
	}
	records = append(records, records[0])
	require.NoError(os.WriteFile(filepath.Join(root, "message.json"), []byte("["+strings.Join(records, ",")+"]"), 0600))
	summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "owner"})
	require.NoError(err)
	assert.EqualValues(maxMessageBatch+1, summary.MessagesAdded)
	assert.EqualValues(2, summary.MessagesSkipped)
	keys := []string{messageKey("group:batch", "message-0"), messageKey("group:batch", fmt.Sprintf("message-%d", maxMessageBatch))}
	ids, err := st.MessageExistsBatchContext(t.Context(), summary.SourceID, keys)
	require.NoError(err)
	for _, key := range keys {
		raw, err := st.GetMessageRawContext(t.Context(), ids[key])
		require.NoError(err)
		var m Message
		require.NoError(json.Unmarshal(raw, &m))
		assert.Equal(key, messageKey("group:batch", m.ID))
		if m.ID == "message-0" {
			assert.Equal("body-0", m.Text)
		}
	}
}
