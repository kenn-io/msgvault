package telegram

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExportReaderFull(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	reader, err := OpenExport("testdata/full")
	require.NoError(err)
	defer func() { require.NoError(reader.Close()) }()
	assert.EqualValues(7000000000000123456, reader.OwnerID())
	assert.Equal("Test Owner", reader.OwnerName())
	var chats []*Chat
	require.NoError(reader.Chats(func(chat *Chat) error { chats = append(chats, chat); return nil }))
	require.Len(chats, 4)
	assert.Equal("user7000000000000555555", chats[0].PeerKey())
	assert.Equal("channel9999999999", chats[1].PeerKey())
	assert.Equal("channel1111111111", chats[2].PeerKey())
	assert.Equal("user7000000000000444444", chats[3].PeerKey())
	assert.Equal("see https://example.org/docs and @alice", chats[0].Messages[1].Text)
	assert.Equal(time.Date(2021, 1, 17, 18, 57, 52, 0, time.UTC), chats[0].Messages[0].SentAt)
}

func TestExportReaderChatsBeforeLeftChats(t *testing.T) {
	root := t.TempDir()
	writeExport(t, root, `{"left_chats":{"list":[{"id":2,"type":"bot_chat"}]},"chats":{"about":"export","list":[{"id":1,"type":"saved_messages"}],"extra":true}}`)
	reader, err := OpenExport(root)
	require.NoError(t, err)
	defer func() { require.NoError(t, reader.Close()) }()
	var peers []string
	require.NoError(t, reader.Chats(func(chat *Chat) error { peers = append(peers, chat.PeerKey()); return nil }))
	assert.Equal(t, []string{"user1", "user2"}, peers)
}

func TestExportReaderMalformed(t *testing.T) {
	for _, tc := range []struct{ name, data, want string }{
		{"missing id", `{"type":"message","date_unixtime":"1","text":""}`, "id must be positive"},
		{"zero id", `{"id":0,"type":"message","date_unixtime":"1","text":""}`, "id must be positive"},
		{"sender", `{"id":1,"type":"message","date_unixtime":"1","from_id":"123","text":""}`, "invalid peer id"},
		{"reply peer", `{"id":1,"type":"message","date_unixtime":"1","reply_to_peer_id":"userx","text":""}`, "invalid peer id"},
		{"array text", `{"id":1,"type":"message","date_unixtime":"1","text":["keep",{"text":123}]}`, "text must be a string"},
		{"missing text", `{"id":1,"type":"message","date_unixtime":"1"}`, "text must be a string"},
		{"null text", `{"id":1,"type":"message","date_unixtime":"1","text":null}`, "text must be a string"},
		{"bad time", `{"id":1,"type":"message","date_unixtime":"bad","text":""}`, "date_unixtime"},
		{"missing time", `{"id":1,"type":"message","date":"2020-01-01T00:00:00","text":""}`, "date_unixtime"},
		{"fractional time", `{"id":1,"type":"message","date_unixtime":"1.5","text":""}`, "date_unixtime"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			root := t.TempDir()
			writeExport(t, root, `{"id":5,"name":"Example chat","type":"personal_chat","messages":[`+tc.data+`]}`)
			reader, err := OpenExport(root)
			require.NoError(err)
			defer func() { require.NoError(reader.Close()) }()
			err = reader.Chats(func(*Chat) error { return nil })
			require.ErrorContains(err, tc.want)
			require.ErrorContains(err, "Example chat")
			require.ErrorContains(err, "message ")
		})
	}
	for _, data := range []string{`{} {}`, `{} trailing`, `[]`, `{"chats":{"list":[]}`} {
		t.Run(data, func(t *testing.T) {
			root := t.TempDir()
			writeExport(t, root, data)
			_, err := OpenExport(root)
			require.Error(t, err)
		})
	}
}
