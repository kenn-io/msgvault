package telegram

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readChats(t *testing.T, root string) (*ExportReader, []Chat) {
	t.Helper()
	r, err := OpenExport(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	var chats []Chat
	for {
		chat, err := r.NextChat()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		chats = append(chats, *chat)
	}
	return r, chats
}

func TestParseFullExport(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	r, chats := readChats(t, filepath.Join("testdata", "full"))

	assert.EqualValues(7000000000000123456, r.OwnerID(), "owner id (large int64)")
	assert.Equal("Test Owner", r.OwnerName(), "owner display name")
	assert.NotEmpty(t, r.Fingerprint(), "export fingerprint")

	require.Len(chats, 4, "chats plus left chats")

	direct := chats[0]
	assert.EqualValues(7000000000000555555, direct.ID)
	assert.Equal("personal_chat", direct.Type)

	require.Len(direct.Messages, 4, "service message retained for classification")
	assert.False(direct.Messages[0].Service)
	assert.Equal("Plain string hello", direct.Messages[0].Text)
	assert.Equal("user7000000000000555555", direct.Messages[0].FromID)
	assert.Equal("Alice Example", direct.Messages[0].FromName)

	// Entity-array text and text_entities flatten in order with no inserted
	// separators; text_entities wins when both are present.
	assert.Equal("see https://example.org/docs and @alice", direct.Messages[1].Text)
	// is_from_me input: from_id equals the owner's numeric id.
	assert.Equal("user7000000000000123456", direct.Messages[1].FromID)

	assert.Equal("photos/photo_1@01-01-2020_12-00-00.jpg", direct.Messages[2].Photo)

	assert.True(direct.Messages[3].Service, "service message classified")

	// date_unixtime (string) preferred; date fallback tolerated.
	group := chats[1]
	assert.EqualValues(-1009999999999, group.ID, "negative group id")
	assert.Equal("public_supergroup", group.Type)
	require.Len(group.Messages, 6)
	assert.Equal("voice_messages/voice_message_1@01-01-2020_12-00-00.ogg", group.Messages[0].File)
	assert.Equal("voice_message", group.Messages[0].MediaType)
	assert.Equal("channel7000000000000777777", group.Messages[1].FromID)
	// A skipped-media warning stays message text.
	assert.Equal("[Photo unavailable. You can download it from the Telegram servers.]", group.Messages[2].Text)

	channel := chats[2]
	assert.EqualValues(-1001111111111, channel.ID)
	assert.Equal("private_channel", channel.Type)

	left := chats[3]
	assert.EqualValues(7000000000000444444, left.ID, "left chat parsed")
	assert.Equal("bot_chat", left.Type)
}

func TestParseSingleChatRoot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	r, chats := readChats(t, filepath.Join("testdata", "single"))

	assert.EqualValues(0, r.OwnerID(), "single-chat export has no owner")
	require.Len(chats, 1)
	chat := chats[0]
	assert.EqualValues(-1008888888888, chat.ID)
	assert.Equal("private_group", chat.Type)
	require.Len(chat.Messages, 2)
	assert.Equal("single chat body", chat.Messages[0].Text)
	assert.Equal("sticker", chat.Messages[1].MediaType, "sticker_emoji implies sticker media")

	// The single chat exhausts the reader.
	_, err := r.NextChat()
	assert.ErrorIs(err, io.EOF)
}

func TestParseMissingResultJSON(t *testing.T) {
	_, err := OpenExport(t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no result.json")
}

func TestParseDateFallbackWithoutUnixtime(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(dir, "result.json"), []byte(`{
		"id": 42, "type": "personal_chat", "name": "n",
		"messages": [{"id": 1, "type": "message", "date": "2021-01-17T18:57:52:00", "text": "x"}]
	}`), 0o644))
	_, chats := readChats(t, dir)
	require.Len(chats, 1)
	require.Len(chats[0].Messages, 1)
	want := time.Date(2021, 1, 17, 18, 57, 52, 0, time.UTC)
	assert.Equal(want, chats[0].Messages[0].SentAt, "date tail trimmed and parsed")
}

// TestParsePreservesReplyEditedAndDocumentMetadata pins the fields the
// importer derives thread links, the edited flag, and document metadata
// from: reply_to_message_id, edited/edited_unixtime, and a file's official
// file_name and mime_type.
func TestParsePreservesReplyEditedAndDocumentMetadata(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(dir, "result.json"), []byte(`{
		"id": 42, "type": "personal_chat", "name": "n",
		"messages": [
			{"id": 1, "type": "message", "date_unixtime": "1610909872", "text": "original"},
			{"id": 2, "type": "message", "date_unixtime": "1610909900", "reply_to_message_id": 1,
			 "edited": "2021-01-17T19:13:19:00", "edited_unixtime": "1610910799", "text": "now edited"},
			{"id": 3, "type": "message", "date_unixtime": "1610909950",
			 "file": "documents/report_1.bin", "file_name": "Quarterly Report.pdf",
			 "mime_type": "application/pdf", "media_type": "document", "text": ""},
			{"id": 4, "type": "message", "date_unixtime": "1610909960", "text": "plain"}
		]
	}`), 0o644))
	_, chats := readChats(t, dir)
	require.Len(chats, 1)
	msgs := chats[0].Messages
	require.Len(msgs, 4)

	assert.Zero(msgs[0].ReplyTo, "a non-reply carries no parent id")
	assert.EqualValues(1, msgs[1].ReplyTo, "reply_to_message_id parses")
	assert.False(msgs[0].Edited)
	assert.True(msgs[1].Edited, "edited_unixtime marks the message edited")
	assert.Equal("Quarterly Report.pdf", msgs[2].FileName, "official file_name parses")
	assert.Equal("application/pdf", msgs[2].MimeType, "official mime_type parses")
	assert.True(msgs[3].Edited == false && msgs[3].FileName == "", "plain message stays plain")
}

func TestParseIgnoresUnrelatedJSONFiles(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(dir, "result.json"), []byte(`{
		"id": 5, "type": "bot_chat", "name": "n", "messages": []
	}`), 0o644))
	require.NoError(os.WriteFile(filepath.Join(dir, "result2.json"), []byte(`{
		"id": 6, "type": "bot_chat", "name": "other", "messages": []
	}`), 0o644))
	require.NoError(os.WriteFile(filepath.Join(dir, "results.json"), []byte(`{}`), 0o644))
	_, chats := readChats(t, dir)
	require.Len(chats, 1, "only result.json parses; numbered files are not continuations")
}

// TestParseFingerprintTracksContent pins the resume guard's input: the
// fingerprint changes when the file at the same path changes.
func TestParseFingerprintTracksContent(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "result.json")
	require.NoError(os.WriteFile(path, []byte(`{
		"id": 5, "type": "bot_chat", "name": "n", "messages": []
	}`), 0o644))
	before, chats := readChats(t, dir)
	require.Len(chats, 1)

	require.NoError(os.WriteFile(path, []byte(`{
		"id": 5, "type": "bot_chat", "name": "n", "messages": [{"id": 1, "type": "message", "text": "x"}]
	}`), 0o644))
	after, _ := readChats(t, dir)
	assert.NotEqual(t, before.Fingerprint(), after.Fingerprint(), "changed bytes change the fingerprint")
}
