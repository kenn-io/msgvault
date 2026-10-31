package telegram

import (
	"bytes"
	"context"
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
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func writeExport(t *testing.T, root, data string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(root, "result.json"), []byte(data), 0600))
}

func TestImportPeerCollision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	writeExport(t, root, `{"personal_information":{"user_id":1},"chats":{"list":[{"id":555,"type":"personal_chat","messages":[{"id":1,"type":"message","from_id":"user1","text":"direct","date_unixtime":"1610909872"}]},{"id":555,"type":"private_supergroup","messages":[{"id":1,"type":"message","from_id":"user2","text":"group","date_unixtime":"1610909872"}]}]}}`)
	summary, err := Import(t.Context(), st, ImportOptions{RootDir: root})
	require.NoError(err)
	assert.EqualValues(2, summary.MessagesAdded)
	var n int
	require.NoError(st.DB().QueryRow("SELECT count(*) FROM conversations").Scan(&n))
	assert.Equal(2, n)
	for _, key := range []string{"user555:1", "channel555:1"} {
		require.NoError(st.DB().QueryRow(st.Rebind("SELECT count(*) FROM messages WHERE source_message_id = ?"), key).Scan(&n))
		assert.Equal(1, n)
	}
}

func TestImportCrossPeerAndLaterReplies(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	writeExport(t, root, `{"personal_information":{"user_id":1},"chats":{"list":[{"id":7,"type":"private_supergroup","messages":[{"id":1,"type":"message","text":"wrong parent","date_unixtime":"1610909872"},{"id":2,"type":"message","reply_to_message_id":1,"reply_to_peer_id":"channel9","date_unixtime":"1610909872","text":""},{"id":3,"type":"message","reply_to_message_id":4,"date_unixtime":"1610909872","text":""}]},{"id":9,"type":"public_channel","messages":[{"id":1,"type":"message","text":"correct parent","date_unixtime":"1610909872"}]}]}}`)
	_, err := Import(t.Context(), st, ImportOptions{RootDir: root})
	require.NoError(err)
	var parent string
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT p.source_message_id FROM messages m JOIN messages p ON p.id = m.reply_to_message_id WHERE m.source_message_id = ?"), "channel7:2").Scan(&parent))
	assert.Equal("channel9:1", parent)
	writeExport(t, root, `{"personal_information":{"user_id":1},"chats":{"list":[{"id":7,"type":"private_supergroup","messages":[{"id":4,"type":"message","text":"later parent","date_unixtime":"1610909872"}]},{"id":8,"type":"personal_chat","messages":[{"id":1,"type":"message","date_unixtime":"invalid","text":""}]}]}}`)
	_, err = Import(t.Context(), st, ImportOptions{RootDir: root})
	require.ErrorContains(err, "date_unixtime")
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT p.source_message_id FROM messages m JOIN messages p ON p.id = m.reply_to_message_id WHERE m.source_message_id = ?"), "channel7:3").Scan(&parent))
	assert.Equal("channel7:4", parent)
}

func TestImportSingleOwnerAndUnknownType(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	_, err := Import(t.Context(), st, ImportOptions{RootDir: "testdata/single"})
	require.ErrorIs(err, ErrNoOwner)
	summary, err := Import(t.Context(), st, ImportOptions{RootDir: "testdata/single", Me: "123"})
	require.NoError(err)
	assert.Positive(summary.MessagesAdded)
	root := t.TempDir()
	writeExport(t, root, `{"id":4,"type":"future_chat","messages":[]}`)
	summary, err = Import(t.Context(), st, ImportOptions{RootDir: root, Me: "123"})
	require.NoError(err)
	assert.Equal(1, summary.ChatsSkipped)
}

func TestImportFailedChatResumes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	writeExport(t, root, `{"personal_information":{"user_id":1},"chats":{"list":[{"id":2,"type":"personal_chat","messages":[{"id":1,"type":"message","text":"first","date_unixtime":"1610909872"}]},{"id":3,"type":"personal_chat","messages":[{"id":1,"type":"message","date_unixtime":"invalid","text":""}]}]}}`)
	summary, err := Import(t.Context(), st, ImportOptions{RootDir: root})
	require.Error(err)
	assert.EqualValues(1, summary.MessagesAdded)
	writeExport(t, root, `{"personal_information":{"user_id":1},"chats":{"list":[{"id":2,"type":"personal_chat","messages":[{"id":1,"type":"message","text":"first","date_unixtime":"1610909872"}]},{"id":3,"type":"personal_chat","messages":[{"id":1,"type":"message","text":"fixed","date_unixtime":"1610909872"}]}]}}`)
	summary, err = Import(t.Context(), st, ImportOptions{RootDir: root})
	require.NoError(err)
	assert.False(summary.WasResumed)
	assert.EqualValues(1, summary.MessagesAdded)
	assert.Equal(2, summary.ChatsProcessed)
	// Cancellation preserves the completed-chat checkpoint too.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = Import(ctx, st, ImportOptions{RootDir: root})
	require.ErrorIs(err, context.Canceled)
}

// cancelWriter interrupts after the first chat has been stored.
type cancelWriter struct{ cancel context.CancelFunc }

func (w cancelWriter) Write(p []byte) (int, error) { w.cancel(); return len(p), nil }

func TestImportCancelledAfterChatResumes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	writeExport(t, root, `{"personal_information":{"user_id":1},"chats":{"list":[{"id":2,"type":"personal_chat","messages":[{"id":1,"type":"message","text":"first","date_unixtime":"1610909872","reply_to_message_id":2},{"id":2,"type":"message","text":"parent","date_unixtime":"1610909872"}]},{"id":3,"type":"personal_chat","messages":[{"id":1,"type":"message","text":"second","date_unixtime":"1610909872"}]}]}}`)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	summary, err := Import(ctx, st, ImportOptions{RootDir: root, Logger: slog.New(slog.NewTextHandler(cancelWriter{cancel}, nil))})
	require.ErrorIs(err, context.Canceled)
	assert.EqualValues(2, summary.MessagesAdded)
	var parent string
	require.NoError(st.DB().QueryRow("SELECT p.source_message_id FROM messages m JOIN messages p ON p.id = m.reply_to_message_id WHERE m.source_message_id = 'user2:1'").Scan(&parent))
	assert.Equal("user2:2", parent)
	summary, err = Import(t.Context(), st, ImportOptions{RootDir: root})
	require.NoError(err)
	assert.True(summary.WasResumed)
	assert.EqualValues(1, summary.MessagesAdded)
	assert.Equal(1, summary.ChatsProcessed)
}

func TestImportReplacementExportAfterFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	writeExport(t, root, `{"personal_information":{"user_id":1},"chats":{"list":[{"id":2,"type":"personal_chat","messages":[{"id":1,"type":"message","date_unixtime":"1610909872","text":""}]},{"id":3,"type":"personal_chat","messages":[{"id":1,"type":"message","date_unixtime":"invalid","text":""}]}]}}`)
	_, err := Import(t.Context(), st, ImportOptions{RootDir: root})
	require.ErrorContains(err, "date_unixtime")
	info, err := os.Stat(filepath.Join(root, resultFile))
	require.NoError(err)
	writeExport(t, root, `{"personal_information":{"user_id":1},"chats":{"list":[{"id":2,"type":"personal_chat","messages":[{"id":1,"type":"message","date_unixtime":"1610909872","text":""},{"id":2,"type":"message","text":"new in completed chat","date_unixtime":"1610909872"}]},{"id":3,"type":"personal_chat","messages":[{"id":1,"type":"message","date_unixtime":"1610909872","text":""}]}]}}`)
	require.NoError(os.Chtimes(filepath.Join(root, resultFile), info.ModTime(), info.ModTime()))
	summary, err := Import(t.Context(), st, ImportOptions{RootDir: root})
	require.NoError(err)
	assert.False(summary.WasResumed)
	assert.EqualValues(2, summary.MessagesAdded)
	var n int
	require.NoError(st.DB().QueryRow("SELECT count(*) FROM messages WHERE source_message_id = 'user2:2'").Scan(&n))
	assert.Equal(1, n)
}

func TestImportMediaFilenameContainsWarningWords(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	writeExport(t, root, `{"id":2,"type":"personal_chat","messages":[{"id":1,"type":"message","file":"files/(File not included).txt","date_unixtime":"1610909872","text":""}]}`)
	require.NoError(os.Mkdir(filepath.Join(root, "files"), 0700))
	require.NoError(os.WriteFile(filepath.Join(root, "files", "(File not included).txt"), []byte("attachment bytes"), 0600))
	opts := ImportOptions{RootDir: root, Me: "1", AttachmentsDir: t.TempDir()}
	summary, err := Import(t.Context(), st, opts)
	require.NoError(err)
	assert.Equal(1, summary.AttachmentsStored)
	var path string
	require.NoError(st.DB().QueryRow("SELECT storage_path FROM attachments").Scan(&path))
	data, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, path))
	require.NoError(err)
	assert.Equal("attachment bytes", string(data))
	var n int
	require.NoError(st.DB().QueryRow("SELECT attachment_count FROM messages").Scan(&n))
	assert.Equal(1, n)
}

func TestImportRepeatedLimitAdvances(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	writeExport(t, root, `{"id":2,"type":"personal_chat","messages":[{"id":1,"type":"message","date_unixtime":"1610909872","text":""},{"id":2,"type":"message","date_unixtime":"1610909872","text":""},{"id":3,"type":"message","date_unixtime":"1610909872","text":""}]}`)
	for i := range 3 {
		summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "1", Limit: 1})
		require.NoError(err)
		assert.EqualValues(1, summary.MessagesAdded)
		assert.Equal(i < 2, summary.LimitReached)
	}
	var n int
	require.NoError(st.DB().QueryRow("SELECT count(*) FROM messages").Scan(&n))
	assert.Equal(3, n)
}

func TestImportEditFailureRollsBackMessage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	writeExport(t, root, `{"id":2,"type":"personal_chat","messages":[{"id":1,"type":"message","edited":"2021-01-17T19:13:19","text":"edited body","date_unixtime":"1610909872"}]}`)
	// A failed edit write models interruption between row persistence and the
	// edit marker. Both must roll back so the append-only retry can recover.
	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(`CREATE FUNCTION fail_edit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'edit interrupted'; END $$`)
		require.NoError(err)
		_, err = st.DB().Exec(`CREATE TRIGGER fail_edit BEFORE UPDATE OF is_edited ON messages FOR EACH ROW EXECUTE FUNCTION fail_edit()`)
		require.NoError(err)
	} else {
		_, err := st.DB().Exec(`CREATE TRIGGER fail_edit BEFORE UPDATE OF is_edited ON messages BEGIN SELECT RAISE(ABORT, 'edit interrupted'); END`)
		require.NoError(err)
	}
	_, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "1"})
	require.ErrorContains(err, "edit interrupted")
	var n int
	require.NoError(st.DB().QueryRow("SELECT count(*) FROM messages").Scan(&n))
	assert.Zero(n)
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`DROP TRIGGER fail_edit ON messages`)
	} else {
		_, err = st.DB().Exec(`DROP TRIGGER fail_edit`)
	}
	require.NoError(err)
	summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "1"})
	require.NoError(err)
	assert.EqualValues(1, summary.MessagesAdded)
	var edited bool
	require.NoError(st.DB().QueryRow("SELECT is_edited FROM messages").Scan(&edited))
	assert.True(edited)
}

func TestImportSameSizeReplacementIgnoresCheckpoint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	data := `{"personal_information":{"user_id":1},"chats":{"list":[{"id":2,"type":"personal_chat","messages":[{"id":1,"type":"message","date_unixtime":"1610909872","text":""}]},{"id":3,"type":"personal_chat","messages":[{"id":1,"type":"message","date_unixtime":"1610909872","text":""}]}]}}`
	writeExport(t, root, data)
	info, err := os.Stat(filepath.Join(root, resultFile))
	require.NoError(err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err = Import(ctx, st, ImportOptions{RootDir: root, Logger: slog.New(slog.NewTextHandler(cancelWriter{cancel}, nil))})
	require.ErrorIs(err, context.Canceled)
	writeExport(t, root, strings.Replace(data, `"id":1,"type":"message"`, `"id":2,"type":"message"`, 1))
	modified := info.ModTime().Add(time.Second)
	require.NoError(os.Chtimes(filepath.Join(root, resultFile), modified, modified))
	summary, err := Import(t.Context(), st, ImportOptions{RootDir: root})
	require.NoError(err)
	assert.False(summary.WasResumed)
	assert.EqualValues(2, summary.MessagesAdded)
}

func TestImportFullAndAppendOnly(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	reader, err := OpenExport(t.Context(), "testdata/full")
	require.NoError(err)
	var chats []*Chat
	require.NoError(reader.Chats(t.Context(), func(c *Chat) error { chats = append(chats, c); return nil }))
	require.NoError(reader.Close())
	chats[0].Messages[0].Edited = "2021-01-18T00:00:00"
	for _, m := range []Message{
		{ID: 105, Type: "message", File: "sticker.webp", MediaType: "sticker", Thumbnail: "thumb.jpg"},
		{ID: 106, Type: "message", File: "(File not included. Change data exporting settings to download.)"},
		{ID: 107, Type: "message", Photo: "missing.jpg"},
		{ID: 108, Type: "unsupported"},
	} {
		m.DateUnixtime = "1610909872"
		m.RawText = jsontext.Value(`""`)
		chats[0].Messages = append(chats[0].Messages, m)
	}
	root := t.TempDir()
	data := struct {
		Owner struct {
			ID    int64  `json:"user_id"`
			First string `json:"first_name"`
			Last  string `json:"last_name"`
		} `json:"personal_information"`
		Chats struct {
			List []*Chat `json:"list"`
		} `json:"chats"`
	}{}
	data.Owner.ID = 7000000000000123456
	data.Owner.First = "Test"
	data.Owner.Last = "Owner"
	data.Chats.List = chats
	encoded, err := json.Marshal(data)
	require.NoError(err)
	writeExport(t, root, string(encoded))
	for _, path := range []string{"photos/photo_1@01-01-2020_12-00-00.jpg", "voice_messages/voice_message_1@01-01-2020_12-00-00.ogg", "sticker.webp", "thumb.jpg"} {
		dest := filepath.Join(root, path)
		require.NoError(os.MkdirAll(filepath.Dir(dest), 0700))
		require.NoError(os.WriteFile(dest, []byte(path), 0600))
	}
	opts := ImportOptions{RootDir: root, AttachmentsDir: t.TempDir()}
	summary, err := Import(t.Context(), st, opts)
	require.NoError(err)
	assert.EqualValues(14, summary.MessagesAdded)
	assert.EqualValues(1, summary.ServiceMessagesSkipped)
	assert.EqualValues(1, summary.UnsupportedEntriesSkipped)
	assert.Equal(4, summary.AttachmentsStored)
	assert.Equal(4, summary.AttachmentsMissing)
	assert.Equal(6, summary.ParticipantsResolved)
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"SELECT count(*) FROM conversations", 4},
		{"SELECT count(*) FROM messages WHERE is_edited = TRUE", 1},
		{"SELECT count(*) FROM messages WHERE is_from_me = TRUE", 1},
		{"SELECT count(*) FROM message_recipients WHERE recipient_type = 'to'", 7},
		{"SELECT count(*) FROM attachments WHERE attachment_role IN ('sticker','thumbnail')", 2},
		{"SELECT count(*) FROM messages WHERE sender_id IS NULL", 4},
		{"SELECT count(*) FROM conversations WHERE message_count > 0", 4},
	} {
		var n int
		require.NoError(st.DB().QueryRow(tc.query).Scan(&n))
		assert.Equal(tc.want, n, tc.query)
	}
	var text string
	var messageID int64
	require.NoError(st.DB().QueryRow("SELECT id FROM messages WHERE source_message_id = 'user7000000000000555555:102'").Scan(&messageID))
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT body_text FROM message_bodies WHERE message_id = ?"), messageID).Scan(&text))
	assert.Equal("see https://example.org/docs and @alice", text)
	// Fully stored rows must survive a re-import even if copying is impossible.
	require.NoError(os.Remove(filepath.Join(root, "sticker.webp")))
	summary, err = Import(t.Context(), st, opts)
	require.NoError(err)
	assert.Zero(summary.MessagesAdded)
	assert.EqualValues(14, summary.MessagesSkipped)
	assert.Zero(summary.AttachmentsStored)
	chats[0].Messages[0].RawText = jsontext.Value(`"replacement"`)
	chats[0].Messages[0].Edited = ""
	chats[0].Messages[2].Photo = "different.jpg"
	chats[0].Messages = append(chats[0].Messages, Message{ID: 109, Type: "message", DateUnixtime: "1610909872", RawText: jsontext.Value(`"new arrival"`)})
	encoded, err = json.Marshal(data)
	require.NoError(err)
	writeExport(t, root, string(encoded))
	summary, err = Import(t.Context(), st, opts)
	require.NoError(err)
	assert.EqualValues(1, summary.MessagesAdded)
	require.NoError(st.DB().QueryRow("SELECT id FROM messages WHERE source_message_id = 'user7000000000000555555:101'").Scan(&messageID))
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT body_text FROM message_bodies WHERE message_id = ?"), messageID).Scan(&text))
	assert.Equal("Plain string hello", text)
	var edited bool
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT is_edited FROM messages WHERE id = ?"), messageID).Scan(&edited))
	assert.True(edited)
	var n int
	require.NoError(st.DB().QueryRow("SELECT count(*) FROM attachments WHERE filename = 'different.jpg'").Scan(&n))
	assert.Zero(n)
}

func TestImportFillsMissingMediaSlots(t *testing.T) {
	for _, initial := range []string{"(File not included. Change data exporting settings to download.)", "old-name.jpg"} {
		t.Run(initial, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			root := t.TempDir()
			data := `{"id":2,"type":"personal_chat","messages":[{"id":1,"type":"message","date_unixtime":"1610909872","from_id":"user2","text":"original","photo":"` + initial + `"}]}`
			writeExport(t, root, data)
			opts := ImportOptions{RootDir: root, Me: "1", AttachmentsDir: t.TempDir()}
			summary, err := Import(t.Context(), st, opts)
			require.NoError(err)
			assert.Equal(1, summary.AttachmentsMissing)
			require.NoError(os.WriteFile(filepath.Join(root, "new-name.jpg"), []byte("photo bytes"), 0600))
			data = strings.Replace(data, initial, "new-name.jpg", 1)
			data = strings.Replace(data, `"text":"original"`, `"text":"changed","edited":"later","from_id":"user3"`, 1)
			// Keep from_id unique in the JSON object.
			data = strings.Replace(data, `"from_id":"user2",`, "", 1)
			writeExport(t, root, data)
			summary, err = Import(t.Context(), st, opts)
			require.NoError(err)
			assert.Zero(summary.MessagesAdded)
			assert.Equal(1, summary.AttachmentsStored)
			var n int
			require.NoError(st.DB().QueryRow("SELECT count(*) FROM attachments").Scan(&n))
			assert.Equal(1, n)
			var id int64
			var edited bool
			require.NoError(st.DB().QueryRow("SELECT id, attachment_count, is_edited FROM messages").Scan(&id, &n, &edited))
			assert.Equal(1, n)
			assert.False(edited)
			var text string
			require.NoError(st.DB().QueryRow(st.Rebind("SELECT body_text FROM message_bodies WHERE message_id = ?"), id).Scan(&text))
			assert.Equal("original", text)
			var sender string
			require.NoError(st.DB().QueryRow("SELECT pi.identifier_value FROM participant_identifiers pi JOIN messages m ON m.sender_id = pi.participant_id WHERE pi.identifier_type = 'telegram'").Scan(&sender))
			assert.Equal("user2", sender)
			var path string
			require.NoError(st.DB().QueryRow("SELECT storage_path FROM attachments").Scan(&path))
			bytes, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, path))
			require.NoError(err)
			assert.Equal("photo bytes", string(bytes))
			require.NoError(os.Remove(filepath.Join(root, "new-name.jpg")))
			summary, err = Import(t.Context(), st, opts)
			require.NoError(err)
			assert.Zero(summary.AttachmentsStored)
		})
	}
}

func TestImportMediaFailureIsRetryable(t *testing.T) {
	for _, kind := range []string{"directory", "symlink", "unreadable", "storage", "escape", "absolute"} {
		t.Run(kind, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			root := t.TempDir()
			media := filepath.Join(root, "photo.jpg")
			path := "photo.jpg"
			opts := ImportOptions{RootDir: root, Me: "1", AttachmentsDir: filepath.Join(t.TempDir(), "attachments")}
			switch kind {
			case "directory":
				require.NoError(os.Mkdir(media, 0700))
			case "symlink":
				target := filepath.Join(t.TempDir(), "photo.jpg")
				require.NoError(os.WriteFile(target, []byte("photo"), 0600))
				require.NoError(os.Symlink(target, media))
			case "unreadable":
				require.NoError(os.WriteFile(media, []byte("photo"), 0000))
			case "storage":
				require.NoError(os.WriteFile(media, []byte("photo"), 0600))
				require.NoError(os.WriteFile(opts.AttachmentsDir, []byte("blocked"), 0600))
			case "escape":
				path = "../outside.jpg"
			case "absolute":
				path = media
			}
			encodedPath, err := json.Marshal(path)
			require.NoError(err)
			writeExport(t, root, `{"id":2,"type":"personal_chat","messages":[{"id":1,"type":"message","date_unixtime":"1","text":"","photo":`+string(encodedPath)+`}]}`)
			summary, err := Import(t.Context(), st, opts)
			sourceMissing := kind != "storage"
			if sourceMissing {
				require.NoError(err)
				assert.EqualValues(1, summary.MessagesAdded)
				assert.Equal(1, summary.AttachmentsMissing)
			} else {
				require.Error(err)
				var n int
				require.NoError(st.DB().QueryRow("SELECT count(*) FROM messages").Scan(&n))
				assert.Zero(n)
			}
			if kind == "escape" || kind == "absolute" {
				writeExport(t, root, `{"id":2,"type":"personal_chat","messages":[{"id":1,"type":"message","date_unixtime":"1","text":"","photo":"photo.jpg"}]}`)
				require.NoError(os.WriteFile(media, []byte("photo"), 0600))
			} else if sourceMissing {
				require.NoError(os.Remove(media))
				require.NoError(os.WriteFile(media, []byte("photo"), 0600))
			} else {
				require.NoError(os.Remove(opts.AttachmentsDir))
			}
			summary, err = Import(t.Context(), st, opts)
			require.NoError(err)
			if sourceMissing {
				assert.Zero(summary.MessagesAdded)
			} else {
				assert.EqualValues(1, summary.MessagesAdded)
			}
			assert.Equal(1, summary.AttachmentsStored)
		})
	}
}

func TestImportFutureCheckpointRefused(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	writeExport(t, root, `{"id":2,"type":"personal_chat","messages":[{"id":1,"type":"message","date_unixtime":"1","text":""},{"id":2,"type":"message","date_unixtime":"1","text":""}]}`)
	opts := ImportOptions{RootDir: root, Me: "1", Limit: 1}
	_, err := Import(t.Context(), st, opts)
	require.NoError(err)
	var payload string
	var id int64
	require.NoError(st.DB().QueryRow("SELECT id,cursor_before FROM sync_runs").Scan(&id, &payload))
	payload = strings.Replace(payload, `"version":1`, `"version":999`, 1)
	_, err = st.DB().Exec(st.Rebind("UPDATE sync_runs SET cursor_before = ? WHERE id = ?"), payload, id)
	require.NoError(err)
	_, err = Import(t.Context(), st, opts)
	require.ErrorContains(err, "--no-resume")
	opts.NoResume = true
	summary, err := Import(t.Context(), st, opts)
	require.NoError(err)
	assert.EqualValues(1, summary.MessagesAdded)
}

func TestImportCompletionErrorsReported(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	writeExport(t, root, `{"id":2,"type":"personal_chat","messages":[{"id":1,"type":"message","date_unixtime":"1","text":"hello"}]}`)
	// Refuse only sync finalization, after the message transaction succeeds.
	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(`CREATE FUNCTION fail_sync_completion() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'completion unavailable'; END $$`)
		require.NoError(err)
		_, err = st.DB().Exec(`CREATE TRIGGER fail_sync_completion BEFORE UPDATE OF status ON sync_runs FOR EACH ROW WHEN (NEW.status = 'completed') EXECUTE FUNCTION fail_sync_completion()`)
		require.NoError(err)
	} else {
		_, err := st.DB().Exec(`CREATE TRIGGER fail_sync_completion BEFORE UPDATE OF status ON sync_runs WHEN NEW.status = 'completed' BEGIN SELECT RAISE(ABORT, 'completion unavailable'); END`)
		require.NoError(err)
	}
	summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "1"})
	require.ErrorContains(err, "completion unavailable")
	assert.EqualValues(1, summary.MessagesAdded)
	var n int
	require.NoError(st.DB().QueryRow("SELECT count(*) FROM messages").Scan(&n))
	assert.Equal(1, n)
}

func TestImportUnsupportedAndEmptyText(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	writeExport(t, root, `{"personal_information":{"user_id":1},"chats":{"list":[{"id":2,"type":"personal_chat","messages":[{"id":1,"type":"message","date_unixtime":"1","text":"before"},{"id":2,"type":"message","date_unixtime":"1","rich_message":{"blocks":[]}},{"type":"future_record"},{"id":3,"type":"message","date_unixtime":"1"}]},{"id":3,"type":"personal_chat","messages":[{"id":1,"type":"message","date_unixtime":"1","text":"after"}]}]}}`)
	summary, err := Import(t.Context(), st, ImportOptions{RootDir: root})
	require.NoError(err)
	assert.EqualValues(3, summary.MessagesAdded)
	assert.EqualValues(2, summary.UnsupportedEntriesSkipped)
	var n int
	require.NoError(st.DB().QueryRow("SELECT count(*) FROM messages WHERE source_message_id = 'user2:3' AND snippet IS NULL").Scan(&n))
	assert.Equal(1, n)
}

func TestImportContactVCard(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	writeExport(t, root, `{"id":2,"type":"personal_chat","messages":[{"id":1,"type":"message","date_unixtime":"1","text":"","contact_vcard":"contact.vcard"}]}`)
	const vcard = "BEGIN:VCARD\nVERSION:3.0\nFN:Example Contact\nEND:VCARD\n"
	require.NoError(os.WriteFile(filepath.Join(root, "contact.vcard"), []byte(vcard), 0600))
	opts := ImportOptions{RootDir: root, Me: "1", AttachmentsDir: t.TempDir()}
	summary, err := Import(t.Context(), st, opts)
	require.NoError(err)
	assert.Equal(1, summary.AttachmentsStored)
	var path string
	require.NoError(st.DB().QueryRow("SELECT storage_path FROM attachments WHERE source_part_key = 'contact_vcard'").Scan(&path))
	data, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, path))
	require.NoError(err)
	assert.Equal(vcard, string(data))
}

func TestImportUnsupportedStructuredEntries(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	writeExport(t, root, `{"id":2,"type":"personal_chat","messages":[{"id":1,"type":"message","date_unixtime":"1","text":"","poll":{"question":"Lunch?"}},{"id":2,"type":"message","date_unixtime":"1","text":"","location_information":{"latitude":0,"longitude":0}},{"id":3,"type":"message","date_unixtime":"1","text":"","contact_information":{"first_name":"Example","phone_number":""}},{"id":4,"type":"message","date_unixtime":"1","text":"forwarded text","forwarded_from":"Example Channel"},{"id":5,"type":"message","date_unixtime":"1","text":"caption","poll":{"question":"Lunch?"}},{"id":6,"type":"message","date_unixtime":"1","text":"","contact_information":{"first_name":"Example"},"contact_vcard":"contact.vcard"}]}`)
	summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "1"})
	require.NoError(err)
	assert.EqualValues(3, summary.UnsupportedEntriesSkipped)
	assert.EqualValues(3, summary.MessagesAdded)
	rows, err := st.DB().Query("SELECT source_message_id FROM messages ORDER BY source_message_id")
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	var keys []string
	for rows.Next() {
		var key string
		require.NoError(rows.Scan(&key))
		keys = append(keys, key)
	}
	require.NoError(rows.Err())
	assert.Equal([]string{"user2:4", "user2:5", "user2:6"}, keys)
	var metadata string
	require.NoError(st.DB().QueryRow("SELECT metadata FROM messages WHERE source_message_id = 'user2:4'").Scan(&metadata))
	assert.JSONEq(`{"forwarded_from":"Example Channel"}`, metadata)
}

func TestImportParticipantNameBackfill(t *testing.T) {
	for _, sender := range []string{"user1", "user3"} {
		t.Run(sender, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			root := t.TempDir()
			writeExport(t, root, `{"id":2,"type":"personal_chat","messages":[{"id":1,"type":"message","date_unixtime":"1","from_id":"`+sender+`","text":"first"},{"id":2,"type":"message","date_unixtime":"2","from_id":"`+sender+`","from":"Example Sender","text":"second"}]}`)
			_, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "1"})
			require.NoError(err)
			var name string
			require.NoError(st.DB().QueryRow(st.Rebind("SELECT p.display_name FROM participants p JOIN participant_identifiers pi ON pi.participant_id = p.id WHERE pi.identifier_type = 'telegram' AND pi.identifier_value = ?"), sender).Scan(&name))
			assert.Equal("Example Sender", name)
		})
	}
}

func TestImportSearchMatchesRebuiltIndex(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	sender, err := st.EnsureParticipantByIdentifierContext(t.Context(), "telegram", "user3", "Senderneedle")
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind("UPDATE participants SET email_address = ? WHERE id = ?"), "searchowner@example.org", sender)
	require.NoError(err)
	root := t.TempDir()
	writeExport(t, root, `{"id":2,"name":"Chatneedle","type":"personal_chat","messages":[{"id":1,"type":"message","date_unixtime":"1","from_id":"user3","from":"Senderneedle","text":"Bodyneedle"}]}`)
	_, err = Import(t.Context(), st, ImportOptions{RootDir: root, Me: "1"})
	require.NoError(err)
	for _, rebuilt := range []bool{false, true} {
		if rebuilt {
			_, err = st.RebuildFTS(nil)
			require.NoError(err)
		}
		for _, tc := range []struct {
			term string
			want int64
		}{{"Chatneedle", 0}, {"Senderneedle", 0}, {"Bodyneedle", 1}, {"searchowner", 1}} {
			_, total, err := st.SearchMessages(tc.term, 0, 100)
			require.NoError(err)
			assert.Equal(tc.want, total, "term %s, rebuilt %t", tc.term, rebuilt)
		}
	}
}

func TestImportInvalidMediaContinuesLaterChats(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	writeExport(t, root, `{"personal_information":{"user_id":1},"chats":{"list":[{"id":2,"type":"personal_chat","messages":[{"id":1,"type":"message","date_unixtime":"1","text":"before","photo":"../outside.jpg"}]},{"id":3,"type":"personal_chat","messages":[{"id":1,"type":"message","date_unixtime":"1","text":"after"}]}]}}`)
	var warnings bytes.Buffer
	summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, AttachmentsDir: t.TempDir(), Logger: slog.New(slog.NewTextHandler(&warnings, nil))})
	require.NoError(err)
	assert.EqualValues(2, summary.MessagesAdded)
	assert.Equal(1, summary.AttachmentsMissing)
	assert.Contains(warnings.String(), "level=WARN")
	var missing int
	require.NoError(st.DB().QueryRow("SELECT count(*) FROM attachments WHERE source_part_key = 'photo' AND (content_hash IS NULL OR content_hash = '')").Scan(&missing))
	assert.Equal(1, missing)
}

func TestImportIgnoresOtherSyncTypeCheckpoint(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	writeExport(t, root, `{"id":2,"type":"personal_chat","messages":[{"id":1,"type":"message","date_unixtime":"1","text":"first"},{"id":2,"type":"message","date_unixtime":"1","text":"second"}]}`)
	opts := ImportOptions{RootDir: root, Me: "1", Limit: 1}
	summary, err := Import(t.Context(), st, opts)
	require.NoError(err)
	foreignSync, err := st.StartSyncContext(t.Context(), summary.SourceID, "sync-telegram")
	require.NoError(err)
	require.NoError(st.PauseSyncWithCheckpoint(foreignSync, &store.Checkpoint{PageToken: `{"version":999}`}))
	summary, err = Import(t.Context(), st, opts)
	require.NoError(err)
	assert.True(summary.WasResumed)
	assert.EqualValues(1, summary.MessagesAdded)
}

func TestImportMediaRecoveryRollsBackWithStats(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	data := `{"id":2,"type":"personal_chat","messages":[{"id":1,"type":"message","date_unixtime":"1","text":"","photo":"photo.jpg"}]}`
	writeExport(t, root, data)
	opts := ImportOptions{RootDir: root, Me: "1", AttachmentsDir: t.TempDir()}
	_, err := Import(t.Context(), st, opts)
	require.NoError(err)
	require.NoError(os.WriteFile(filepath.Join(root, "photo.jpg"), []byte("photo"), 0600))
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`CREATE FUNCTION fail_media_stats() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'stats unavailable'; END $$`)
		require.NoError(err)
		_, err = st.DB().Exec(`CREATE TRIGGER fail_media_stats BEFORE UPDATE OF attachment_count ON messages FOR EACH ROW EXECUTE FUNCTION fail_media_stats()`)
		require.NoError(err)
	} else {
		_, err = st.DB().Exec(`CREATE TRIGGER fail_media_stats BEFORE UPDATE OF attachment_count ON messages BEGIN SELECT RAISE(ABORT, 'stats unavailable'); END`)
		require.NoError(err)
	}
	_, err = Import(t.Context(), st, opts)
	require.ErrorContains(err, "stats unavailable")
	var path string
	require.NoError(st.DB().QueryRow("SELECT storage_path FROM attachments").Scan(&path))
	assert.Empty(path)
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`DROP TRIGGER fail_media_stats ON messages`)
	} else {
		_, err = st.DB().Exec(`DROP TRIGGER fail_media_stats`)
	}
	require.NoError(err)
	summary, err := Import(t.Context(), st, opts)
	require.NoError(err)
	assert.Equal(1, summary.AttachmentsStored)
	require.NoError(st.DB().QueryRow("SELECT storage_path FROM attachments").Scan(&path))
	assert.NotEmpty(path)
}
