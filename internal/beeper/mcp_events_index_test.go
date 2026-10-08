package beeper

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMCPBeeperIndexFailureKeepsArchiveAndProgress(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFakeBeeper(t)
	chat := nativeEventsChat("index-failure-account", "!index-failure:example.test")
	f.addChat(chat)
	imp, st, done := newTestImporter(t, f)
	defer done()
	enableNativeBeeperEvents(t, st)
	opts := ImportOptions{AccountID: chat.AccountID, NoMedia: true}
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`CREATE FUNCTION reject_beeper_derived_index_fn() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.source_message_id='unindexed-live-message' AND NEW.search_fts IS NOT NULL THEN RAISE EXCEPTION 'synthetic derived index failure'; END IF; RETURN NEW; END $$`)
		require.NoError(err)
		_, err = st.DB().Exec(`CREATE TRIGGER reject_beeper_derived_index BEFORE UPDATE OF search_fts ON messages FOR EACH ROW EXECUTE FUNCTION reject_beeper_derived_index_fn()`)
	} else {
		_, err = st.DB().Exec(`DROP TABLE messages_fts`)
	}
	require.NoError(err)
	body := "Synthetic readable message with unavailable search index"
	f.appendMsg(chat.ID, fakeMsg{ID: "unindexed-live-message", SortKey: 10, Timestamp: time.Now().UTC(), Text: body, SenderID: "@events-sender:example.test", SenderName: "Synthetic Sender"})
	f.appendMsg(chat.ID, fakeMsg{ID: "healthy-live-message", SortKey: 11, Timestamp: time.Now().UTC(), Text: "Synthetic healthy arrival", SenderID: "@events-sender:example.test", SenderName: "Synthetic Sender"})
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err, "a derived index failure must not reject the archive or block sync")
	var id int64
	require.NoError(st.DB().QueryRow(`SELECT id FROM messages WHERE source_message_id='unindexed-live-message'`).Scan(&id))
	storedBody, err := st.GetMessageBodyText(id)
	require.NoError(err)
	assert.Equal(body, storedBody)
	raw, err := st.GetMessageRaw(id)
	require.NoError(err)
	assert.Contains(string(raw), body)
	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(2, count)
	if st.IsPostgreSQL() {
		var unindexed bool
		require.NoError(st.DB().QueryRow(`SELECT search_fts IS NULL FROM messages WHERE id=$1`, id).Scan(&unindexed))
		assert.True(unindexed, "fixture must reject the derived index write")
		var indexed bool
		require.NoError(st.DB().QueryRow(`SELECT search_fts IS NOT NULL FROM messages WHERE source_message_id='healthy-live-message'`).Scan(&indexed))
		assert.True(indexed, "a failed index must not poison later writes")
	}
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(2, count, "replay must not duplicate occurrences")
	f.appendMsg(chat.ID, fakeMsg{ID: "later-live-message", SortKey: 12, Timestamp: time.Now().UTC(), Text: "Synthetic later arrival", SenderID: "@events-sender:example.test", SenderName: "Synthetic Sender"})
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(3, count, "sync must retain progress after the index failure")
}
