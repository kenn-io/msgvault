package beeper

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMCPBeeperNativeAccountsRemainIsolated(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFakeBeeper(t)
	first := nativeEventsChat("synthetic-account-first", "!first:example.test")
	second := nativeEventsChat("synthetic-account-second", "!second:example.test")
	f.addChat(first)
	f.addChat(second)
	imp, st, done := newTestImporter(t, f)
	defer done()
	enableNativeBeeperEvents(t, st)
	for _, chat := range []*fakeChat{first, second} {
		_, err := imp.Import(t.Context(), ImportOptions{AccountID: chat.AccountID, NoMedia: true})
		require.NoError(err)
		f.appendMsg(chat.ID, fakeMsg{ID: "same-native-message-id", SortKey: 100, Timestamp: time.Now().UTC(), Text: "Synthetic account message", SenderID: "@events-sender:example.test", SenderName: "Synthetic Sender"})
	}
	_, err := imp.Import(t.Context(), ImportOptions{AccountID: first.AccountID, NoMedia: true})
	require.NoError(err)
	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	require.Equal(1, count, "importing one account must not capture the other account's pending head")
	_, err = imp.Import(t.Context(), ImportOptions{AccountID: second.AccountID, NoMedia: true})
	require.NoError(err)
	for _, chat := range []*fakeChat{first, second} {
		var sourceID int64
		require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM sources WHERE source_type='beeper' AND identifier=?`), chat.AccountID).Scan(&sourceID))
		require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM mcp_event_log WHERE source_id=?`), sourceID).Scan(&count))
		assert.Equal(1, count, "each same-network account gets its own conversation occurrence")
	}
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_message_id=?`), "same-native-message-id").Scan(&count))
	assert.Equal(2, count, "native message identifiers are scoped to their account")
}

func TestMCPBeeperNativeReactionReplayAndRecovery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFakeBeeper(t)
	f.pageSize = 1
	chat := nativeEventsChat("synthetic-reaction-account", "!reaction:example.test")
	f.addChat(chat)
	imp, st, done := newTestImporter(t, f)
	defer done()
	enableNativeBeeperEvents(t, st)
	opts := ImportOptions{AccountID: chat.AccountID, NoMedia: true, Limit: 1}
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	var targetCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_message_id=?`), "history-0").Scan(&targetCount))
	require.Zero(targetCount, "the reaction target has not yet been archived")
	f.chat(chat.ID).Msgs[0].Reactions = []map[string]any{
		{"id": "@events-reader:example.test👍", "participantID": "@events-reader:example.test", "reactionKey": "👍", "emoji": true},
		{"id": "@events-reader:example.test❤️", "participantID": "@events-reader:example.test", "reactionKey": "❤️", "emoji": true},
	}
	f.appendMsg(chat.ID, fakeMsg{ID: "synthetic-native-reaction", SortKey: 100, Timestamp: time.Now().UTC(), Type: "REACTION", IsHidden: true, LinkedMessageID: "history-0", SenderID: "@events-reader:example.test", SenderName: "Synthetic Reader"})
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	var reactions, messages int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE kind='reaction'`).Scan(&reactions))
	assert.Zero(reactions, "a live signal does not attribute the recovered embedded reaction set")
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE kind='message'`).Scan(&messages))
	assert.Zero(messages, "recovering the old target is not a new live message")
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM reactions`).Scan(&targetCount))
	assert.Equal(2, targetCount, "historical reaction snapshots are still archived")
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&reactions))
	assert.Zero(reactions, "replaying the same native reaction stays muted")

	f.appendMsg(chat.ID, fakeMsg{ID: "synthetic-history-recovery", SortKey: 200, Timestamp: time.Now().UTC(), Text: "Synthetic full-history recovery", SenderID: "@events-sender:example.test", SenderName: "Synthetic Sender"})
	opts.Full = true
	opts.Limit = 0
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&reactions))
	assert.Zero(reactions, "explicit full-history recovery remains muted")
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_message_id=?`), "synthetic-history-recovery").Scan(&messages))
	assert.Equal(1, messages, "muting history must not prevent the archive import")
}

func TestMCPBeeperCompletedEmptyChatRemainsHistory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFakeBeeper(t)
	chat := nativeEventsChat("empty-events-account", "!empty:example.test")
	chat.Msgs = nil
	f.addChat(chat)
	imp, st, done := newTestImporter(t, f)
	defer done()
	enableNativeBeeperEvents(t, st)
	opts := ImportOptions{AccountID: chat.AccountID, NoMedia: true}
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	f.appendMsg(chat.ID, fakeMsg{ID: "first-after-empty", SortKey: 1, Timestamp: time.Now().UTC(), Text: "Synthetic first message", SenderID: "@events-sender:example.test"})
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Zero(count, "an empty chat has no established head proving live provenance")
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
	assert.Equal(1, count)
}
