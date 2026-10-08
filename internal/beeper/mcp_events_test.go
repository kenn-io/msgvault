package beeper

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func nativeEventsChat(account, chatID string) *fakeChat {
	base := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	chat := &fakeChat{ID: chatID, AccountID: account, Network: "Signal", Title: "Synthetic Events Chat", Type: "group", Participants: []map[string]any{
		{"id": "@events-owner:example.test", "fullName": "Synthetic Owner", "isSelf": true},
		{"id": "@events-sender:example.test", "fullName": "Synthetic Sender"},
		{"id": "@events-reader:example.test", "fullName": "Synthetic Reader"},
	}}
	for i := range 4 {
		chat.Msgs = append(chat.Msgs, fakeMsg{ID: "history-" + strconv.Itoa(i), SortKey: i, Timestamp: base.Add(time.Duration(i) * time.Minute), Text: "Synthetic archived history", SenderID: "@events-sender:example.test", SenderName: "Synthetic Sender"})
	}
	chat.LastActivity = chat.Msgs[len(chat.Msgs)-1].Timestamp
	return chat
}

func enableNativeBeeperEvents(t *testing.T, st *store.Store) {
	t.Helper()
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "synthetic-owner", Capabilities: []store.MCPEventCapability{
		{Family: "msgvault.message_archived", SourceType: "beeper", Kinds: []string{"message", "reaction"}},
	}})
	require.NoError(t, err)
}

func TestMCPBeeperNativeIncrementalPublishesCompleteArchive(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFakeBeeper(t)
	chat := nativeEventsChat("native-events-account", "!events:example.test")
	f.addChat(chat)
	imp, st, done := newTestImporter(t, f)
	defer done()
	enableNativeBeeperEvents(t, st)
	opts := ImportOptions{AccountID: chat.AccountID, NoMedia: true}
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	require.Zero(count, "initial history must be muted")
	f.appendMsg(chat.ID, fakeMsg{ID: "native-live-message", SortKey: 10, Timestamp: time.Now().UTC(), Text: "Complete synthetic live message", SenderID: "@events-sender:example.test", SenderName: "Synthetic Sender", Mentions: []string{"@events-reader:example.test"}})
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	require.Equal(1, count, "one committed message produces one conversation occurrence")
	var messageID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_message_id=?`), "native-live-message").Scan(&messageID))
	var text string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id=?`), messageID).Scan(&text))
	assert.Equal("Complete synthetic live message", text)
	var rawFormat string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT raw_format FROM message_raw WHERE message_id=?`), messageID).Scan(&rawFormat))
	assert.Equal("beeper_json", rawFormat)
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM message_recipients WHERE message_id=? AND recipient_type='mention'`), messageID).Scan(&count))
	assert.Equal(1, count, "mandatory recipient snapshot accompanies the event")
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(1, count, "native replay and reconciliation must not duplicate the occurrence")
}

func TestMCPBeeperMandatoryRawFailureRollsBackArchive(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFakeBeeper(t)
	chat := nativeEventsChat("native-failure-account", "!failure:example.test")
	f.addChat(chat)
	imp, st, done := newTestImporter(t, f)
	defer done()
	enableNativeBeeperEvents(t, st)
	trigger := `CREATE TRIGGER reject_native_beeper_raw BEFORE INSERT ON message_raw BEGIN SELECT RAISE(ABORT,'synthetic mandatory raw failure'); END`
	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(`CREATE FUNCTION reject_native_beeper_raw_fn() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic mandatory raw failure'; END $$`)
		require.NoError(err)
		trigger = `CREATE TRIGGER reject_native_beeper_raw BEFORE INSERT ON message_raw FOR EACH ROW EXECUTE FUNCTION reject_native_beeper_raw_fn()`
	}
	_, err := st.DB().Exec(trigger)
	require.NoError(err)
	_, err = imp.Import(t.Context(), ImportOptions{AccountID: chat.AccountID, NoMedia: true})
	require.ErrorContains(err, "synthetic mandatory raw failure")
	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
	assert.Zero(count, "a mandatory raw failure cannot leave an incomplete message visible")
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Zero(count)
}

func TestMCPBeeperInterleavedHistoryKeepsLiveHead(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFakeBeeper(t)
	f.pageSize = 2
	chat := nativeEventsChat("interleaved-events-account", "!interleaved:example.test")
	f.addChat(chat)
	imp, st, done := newTestImporter(t, f)
	defer done()
	enableNativeBeeperEvents(t, st)
	opts := ImportOptions{AccountID: chat.AccountID, NoMedia: true, Limit: 2}
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
	require.Equal(2, count, "historical backlog remains after the first limited page")
	f.appendMsg(chat.ID, fakeMsg{ID: "interleaved-live-message", SortKey: 100, Timestamp: time.Now().UTC(), Text: "Synthetic live head during backfill", SenderID: "@events-sender:example.test", SenderName: "Synthetic Sender"})
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(1, count, "an established live cursor continues while older history is incomplete")
	opts.Limit = 0
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(1, count, "completing history never relabels old messages as live")
}

func TestMCPBeeperSnapshotPreservesIndependentLabels(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFakeBeeper(t)
	chat := nativeEventsChat("labels-events-account", "!labels:example.test")
	f.addChat(chat)
	imp, st, done := newTestImporter(t, f)
	defer done()
	enableNativeBeeperEvents(t, st)
	opts := ImportOptions{AccountID: chat.AccountID, NoMedia: true}
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	f.appendMsg(chat.ID, fakeMsg{ID: "independently-labeled-message", SortKey: 10, Timestamp: time.Now().UTC(), Text: "Synthetic message with retained label", SenderID: "@events-sender:example.test", SenderName: "Synthetic Sender"})
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	var messageID, sourceID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id,source_id FROM messages WHERE source_message_id=?`), "independently-labeled-message").Scan(&messageID, &sourceID))
	labelID, err := st.EnsureLabel(sourceID, "synthetic-independent-label", "Synthetic retained label", "user")
	require.NoError(err)
	require.NoError(st.AddMessageLabels(messageID, []int64{labelID}))
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM message_labels WHERE message_id=? AND label_id=?`), messageID, labelID).Scan(&count))
	assert.Equal(1, count, "native reconciliation preserves labels outside the importer snapshot")
}

func TestMCPBeeperSnapshotCarriesCompleteAndAdditiveRoster(t *testing.T) {
	for _, complete := range []bool{true, false} {
		t.Run(strconv.FormatBool(complete), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newFakeBeeper(t)
			chat := nativeEventsChat("roster-account", "!roster:example.test")
			f.addChat(chat)
			imp, st, done := newTestImporter(t, f)
			defer done()
			sum, err := imp.Import(t.Context(), ImportOptions{AccountID: chat.AccountID, NoMedia: true})
			require.NoError(err)
			var conversationID int64
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM conversations WHERE source_id=?`), sum.SourceID).Scan(&conversationID))
			reader, err := imp.res.resolveID("@events-reader:example.test", "Synthetic Reader")
			require.NoError(err)
			prior, err := imp.res.resolveID("@prior-member:example.test", "Synthetic Prior Member")
			require.NoError(err)
			require.NoError(st.ReplaceConversationParticipants(conversationID, []store.ConversationParticipantRef{{ParticipantID: prior, Role: "member"}}))
			cc := &chatScope{chatID: chat.ID, convID: conversationID, sourceID: sum.SourceID, opts: ImportOptions{AccountID: chat.AccountID, NoMedia: true}, cs: &ChatState{}, membershipComplete: complete,
				conversation: &store.ConversationPersistData{SourceConversationID: chat.ID, ConversationType: "group", Title: chat.Title, PreserveExistingType: true, PreserveExistingParticipants: !complete,
					Participants: []store.ConversationParticipantRef{{ParticipantID: reader, Role: "admin"}}},
			}
			message := &Message{ID: "roster-message", SenderID: "@events-reader:example.test", Timestamp: time.Now().UTC(), Type: "TEXT", Text: "Synthetic roster snapshot"}
			require.NoError(imp.persistMessage(t.Context(), cc, message, &ImportSummary{}))
			var count int
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM conversation_participants WHERE conversation_id=? AND participant_id=? AND role='admin'`), conversationID, reader).Scan(&count))
			assert.Equal(1, count, "the required roster is carried by the message snapshot")
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM conversation_participants WHERE conversation_id=? AND participant_id=?`), conversationID, prior).Scan(&count))
			wantPrior := 1
			if complete {
				wantPrior = 0
			}
			assert.Equal(wantPrior, count, "only complete membership replaces the prior roster")
		})
	}
}
