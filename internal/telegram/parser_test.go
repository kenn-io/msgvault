package telegram

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestExportReaderCancelled(t *testing.T) {
	for _, root := range []string{"testdata/full", "testdata/single"} {
		t.Run(root, func(t *testing.T) {
			require := require.New(t)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err := OpenExport(ctx, root)
			require.ErrorIs(err, context.Canceled)
			reader, err := OpenExport(t.Context(), root)
			require.NoError(err)
			defer func() { require.NoError(reader.Close()) }()
			visits := 0
			err = reader.Chats(ctx, func(*Chat) error { visits++; return nil })
			require.ErrorIs(err, context.Canceled)
			assert.Zero(t, visits)
		})
	}
}

func TestExportReaderFull(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	reader, err := OpenExport(t.Context(), "testdata/full")
	require.NoError(err)
	defer func() { require.NoError(reader.Close()) }()
	assert.EqualValues(7000000000000123456, reader.OwnerID())
	assert.Equal("Test Owner", reader.OwnerName())
	var chats []*Chat
	require.NoError(reader.Chats(t.Context(), func(chat *Chat) error { chats = append(chats, chat); return nil }))
	require.Len(chats, 4)
	assert.Equal("user7000000000000555555", chats[0].PeerKey())
	assert.Equal("channel9999999999", chats[1].PeerKey())
	assert.Equal("channel1111111111", chats[2].PeerKey())
	assert.Equal("user7000000000000444444", chats[3].PeerKey())
	assert.Equal("see https://example.org/docs and @alice", chats[0].Messages[1].Text)
	assert.Equal(time.Date(2021, 1, 17, 18, 57, 52, 0, time.UTC), chats[0].Messages[0].SentAt)
	root := t.TempDir()
	testutil.WriteFile(t, root, "result.json", []byte(`{"id":2,"type":"personal_chat","messages":[{"id":1,"type":"message","date_unixtime":"1","text_entities":[{"type":"plain","text":"see "},{"type":"text_link","text":"docs","href":"https://example.org/linkneedle"},{"type":"plain","text":" today"}]}]}`))
	linked, err := OpenExport(t.Context(), root)
	require.NoError(err)
	defer func() { require.NoError(linked.Close()) }()
	require.NoError(linked.Chats(t.Context(), func(chat *Chat) error {
		require.Len(chat.Messages, 1)
		assert.Equal("see docs (https://example.org/linkneedle) today", chat.Messages[0].Text)
		return nil
	}))
}

func TestExportReaderChatsBeforeLeftChats(t *testing.T) {
	root := t.TempDir()
	testutil.WriteFile(t, root, "result.json", []byte(`{"left_chats":{"list":[{"id":2,"type":"bot_chat"}]},"chats":{"about":"export","list":[{"id":1,"type":"saved_messages"}],"extra":true}}`))
	reader, err := OpenExport(t.Context(), root)
	require.NoError(t, err)
	defer func() { require.NoError(t, reader.Close()) }()
	var peers []string
	require.NoError(t, reader.Chats(t.Context(), func(chat *Chat) error { peers = append(peers, chat.PeerKey()); return nil }))
	assert.Equal(t, []string{"user1", "user2"}, peers)
}

func TestExportReaderMalformed(t *testing.T) {
	for _, tc := range []struct{ name, data, want string }{
		{"missing id", `{"type":"message","date_unixtime":"1","text":""}`, "id must be nonzero"},
		{"sender", `{"id":1,"type":"message","date_unixtime":"1","from_id":"123","text":""}`, "invalid peer id"},
		{"reply peer", `{"id":1,"type":"message","date_unixtime":"1","reply_to_peer_id":"userx","text":""}`, "invalid peer id"},
		{"array text", `{"id":1,"type":"message","date_unixtime":"1","text":["keep",{"text":123}]}`, "text must be a string"},
		{"null text", `{"id":1,"type":"message","date_unixtime":"1","text":null}`, "text must be a string"},
		{"bad time", `{"id":1,"type":"message","date_unixtime":"bad","text":""}`, "date_unixtime"},
		{"missing time", `{"id":1,"type":"message","date":"2020-01-01T00:00:00","text":""}`, "date_unixtime"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			root := t.TempDir()
			testutil.WriteFile(t, root, "result.json", []byte(`{"id":5,"name":"Example chat","type":"personal_chat","messages":[`+tc.data+`]}`))
			reader, err := OpenExport(t.Context(), root)
			require.NoError(err)
			defer func() { require.NoError(reader.Close()) }()
			err = reader.Chats(t.Context(), func(*Chat) error { return nil })
			require.ErrorContains(err, tc.want)
			require.ErrorContains(err, "Example chat")
			require.ErrorContains(err, "message ")
		})
	}
	for _, tc := range []struct{ data, want string }{
		{`{} {}`, "trailing content"},
		{`{} trailing`, "trailing content"},
		{`[]`, "expected JSON object"},
		{`{"chats":{"list":[]}`, "unexpected EOF"},
	} {
		t.Run(tc.data, func(t *testing.T) {
			root := t.TempDir()
			testutil.WriteFile(t, root, "result.json", []byte(tc.data))
			_, err := OpenExport(t.Context(), root)
			require.ErrorContains(t, err, tc.want)
		})
	}
}
