package telegram

import (
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
	for range 3 {
		summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "1", Limit: 1})
		require.NoError(err)
		assert.EqualValues(1, summary.MessagesAdded)
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
	reader, err := OpenExport("testdata/full")
	require.NoError(err)
	var chats []*Chat
	require.NoError(reader.Chats(func(c *Chat) error { chats = append(chats, c); return nil }))
	require.NoError(reader.Close())
	// Exclude the fixture's two invalid media paths from this successful import.
	chats[1].Messages = append(chats[1].Messages[:3], chats[1].Messages[5:]...)
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
	assert.EqualValues(12, summary.MessagesAdded)
	assert.EqualValues(1, summary.ServiceMessagesSkipped)
	assert.EqualValues(1, summary.UnsupportedEntriesSkipped)
	assert.Equal(4, summary.AttachmentsStored)
	assert.Equal(2, summary.AttachmentsMissing)
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
	assert.EqualValues(12, summary.MessagesSkipped)
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
	for _, initial := range []string{"(Photo not included. Change data exporting settings to download.)", "old-name.jpg"} {
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
	for _, kind := range []string{"source", "storage", "escape", "absolute"} {
		t.Run(kind, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			root := t.TempDir()
			media := filepath.Join(root, "photo.jpg")
			path := "photo.jpg"
			opts := ImportOptions{RootDir: root, Me: "1", AttachmentsDir: filepath.Join(t.TempDir(), "attachments")}
			switch kind {
			case "source":
				require.NoError(os.Mkdir(media, 0700))
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
			_, err = Import(t.Context(), st, opts)
			require.Error(err)
			var n int
			require.NoError(st.DB().QueryRow("SELECT count(*) FROM messages").Scan(&n))
			assert.Zero(n)
			if kind == "escape" || kind == "absolute" {
				return
			}
			if kind == "source" {
				require.NoError(os.Remove(media))
				require.NoError(os.WriteFile(media, []byte("photo"), 0600))
			} else {
				require.NoError(os.Remove(opts.AttachmentsDir))
			}
			summary, err := Import(t.Context(), st, opts)
			require.NoError(err)
			assert.EqualValues(1, summary.MessagesAdded)
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
