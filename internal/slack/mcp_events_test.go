package slack

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func nativeEventsWorkspace(t *testing.T) *fakeSlack {
	t.Helper()
	f := newFakeSlack(t)
	f.users = []map[string]any{
		{"id": "UME", "name": "owner", "real_name": "Synthetic Owner", "profile": map[string]any{"email": "owner@example.test"}},
		{"id": "USENDER", "name": "sender", "real_name": "Synthetic Sender", "profile": map[string]any{"email": "sender@example.test"}},
	}
	f.convs = []*fakeConv{{ID: "CEVENTS", Name: "synthetic-events", Kind: "public", Members: []string{"UME", "USENDER"}, Msgs: []fakeMsg{{
		TS: ts(0), User: "USENDER", Text: "Synthetic message for <@UME>", Edited: true,
		Reactions: []map[string]any{{"name": "wave", "users": []string{"UME"}, "count": 1}},
		Files: []map[string]any{{"id": "FSYNTHETIC", "name": "synthetic.txt", "mimetype": "text/plain", "size": 4,
			"url_private": "https://files.slack.com/files-pri/TSYNTHETIC-FSYNTHETIC/synthetic.txt"}},
	}}}}
	return f
}

// A failure in any required snapshot must leave no partial message for the
// parent's raw-completion shortcut or the Events reader to mistake as ready.
func TestMCPNativeSlackSnapshotRollback(t *testing.T) {
	for _, table := range []string{"message_raw", "message_recipients", "reactions", "attachments"} {
		t.Run(table, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := nativeEventsWorkspace(t)
			imp, opts := testImporter(t, f)
			opts.NoThreads = true
			release := failNativeSlackInsert(t, imp.store, table)
			_, err := imp.Import(t.Context(), opts)
			require.Error(err)
			var count int
			require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
			assert.Zero(count, "a failed mandatory snapshot must roll back the message row")
			require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM message_raw`).Scan(&count))
			assert.Zero(count)
			release()
			_, err = imp.Import(t.Context(), opts)
			require.NoError(err)
			require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
			assert.Equal(1, count)
			require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM message_raw`).Scan(&count))
			assert.Equal(1, count)
			require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM reactions`).Scan(&count))
			assert.Equal(1, count)
			require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&count))
			assert.Equal(1, count, "the retry must retain a pending media marker")
			var edited bool
			require.NoError(imp.store.DB().QueryRow(`SELECT is_edited FROM messages`).Scan(&edited))
			assert.True(edited)
		})
	}
}

// Both backends reject actual production writes, rather than substituting a
// fake Store. The table names come only from the closed test table above.
func failNativeSlackInsert(t *testing.T, st *store.Store, table string) func() {
	t.Helper()
	require := require.New(t)
	name := "fail_native_slack_" + table
	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN RAISE EXCEPTION 'synthetic snapshot failure'; END; $$`, name))
		require.NoError(err)
		_, err = st.DB().Exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON %s FOR EACH ROW EXECUTE FUNCTION %s()`, name, table, name))
		require.NoError(err)
	} else {
		_, err := st.DB().Exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON %s BEGIN SELECT RAISE(ABORT, 'synthetic snapshot failure'); END`, name, table))
		require.NoError(err)
	}
	release := func() {
		if st.IsPostgreSQL() {
			_, err := st.DB().Exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON %s`, name, table))
			require.NoError(err)
			_, err = st.DB().Exec(fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, name))
			require.NoError(err)
		} else {
			_, err := st.DB().Exec(`DROP TRIGGER IF EXISTS ` + name)
			require.NoError(err)
		}
	}
	t.Cleanup(release)
	return release
}

// Raw JSON is also a completion marker. A reader must never observe it on a
// reply whose native parent identity has not been resolved yet.
func TestMCPNativeSlackReplySnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := nativeEventsWorkspace(t)
	parent := &f.convs[0].Msgs[0]
	parent.Replies = []fakeMsg{{TS: ts(1), ThreadTS: parent.TS, User: "UME", Text: "Synthetic reply"}}
	imp, opts := testImporter(t, f)
	const name = "require_native_slack_reply_link"
	if imp.store.IsPostgreSQL() {
		_, err := imp.store.DB().Exec(`CREATE FUNCTION require_native_slack_reply_link() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
		IF EXISTS (SELECT 1 FROM messages WHERE id = NEW.message_id AND metadata->>'slack_thread_ts' != '' AND metadata->>'slack_thread_ts' != metadata->>'slack_message_ts' AND reply_to_message_id IS NULL) THEN
		RAISE EXCEPTION 'reply link must accompany raw archive';
		END IF; RETURN NEW; END; $$`)
		require.NoError(err)
		_, err = imp.store.DB().Exec(`CREATE TRIGGER require_native_slack_reply_link BEFORE INSERT ON message_raw FOR EACH ROW EXECUTE FUNCTION require_native_slack_reply_link()`)
		require.NoError(err)
		t.Cleanup(func() {
			_, err := imp.store.DB().Exec(`DROP TRIGGER IF EXISTS require_native_slack_reply_link ON message_raw`)
			require.NoError(err)
			_, err = imp.store.DB().Exec(`DROP FUNCTION IF EXISTS require_native_slack_reply_link()`)
			require.NoError(err)
		})
	} else {
		_, err := imp.store.DB().Exec(`CREATE TRIGGER require_native_slack_reply_link BEFORE INSERT ON message_raw
		WHEN EXISTS (SELECT 1 FROM messages WHERE id = NEW.message_id AND json_extract(metadata, '$.slack_thread_ts') != '' AND json_extract(metadata, '$.slack_thread_ts') != json_extract(metadata, '$.slack_message_ts') AND reply_to_message_id IS NULL)
		BEGIN SELECT RAISE(ABORT, 'reply link must accompany raw archive'); END`)
		require.NoError(err)
		t.Cleanup(func() {
			_, err := imp.store.DB().Exec(`DROP TRIGGER IF EXISTS ` + name)
			require.NoError(err)
		})
	}
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err, "reply identity must resolve before the complete raw snapshot")
	var linked int
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM messages child WHERE EXISTS (SELECT 1 FROM messages parent WHERE parent.id = child.reply_to_message_id AND parent.source_id = child.source_id)`).Scan(&linked))
	assert.Equal(1, linked)
}

func TestMCPNativeSlackMembershipSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := nativeEventsWorkspace(t)
	imp, opts := testImporter(t, f)
	opts.NoThreads = true
	f.onHistory = func(channelID string) {
		var conversationID int64
		if !assert.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT id FROM conversations WHERE source_conversation_id = ?`), channelID).Scan(&conversationID)) {
			return
		}
		// Simulate another archive writer changing the roster after provider
		// preparation. The message must carry its resolved roster into commit.
		assert.NoError(imp.store.ReplaceConversationParticipants(conversationID, []store.ConversationParticipantRef{}))
	}
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	var members int
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM conversation_participants WHERE EXISTS (SELECT 1 FROM messages m WHERE m.conversation_id = conversation_participants.conversation_id)`).Scan(&members))
	assert.Equal(2, members, "a complete message snapshot must carry the resolved conversation roster")
}

func TestMCPNativeSlackAuxiliaryFailureRetriesParent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, _ := oldThreadWorkspace(t)
	imp, opts := testImporter(t, f)
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	rootTS, replyTS := tsFresh(0), tsFresh(1)
	f.mu.Lock()
	f.conv("C09").Msgs = append(f.conv("C09").Msgs, fakeMsg{TS: rootTS, User: "UME", Text: "Synthetic recovered parent",
		Files: []map[string]any{{"id": "FRECOVERY", "name": "synthetic.txt", "mimetype": "text/plain", "size": 4,
			"url_private": "https://files.slack.com/files-pri/TSYNTHETIC-FRECOVERY/synthetic.txt"}},
		Replies: []fakeMsg{{TS: replyTS, ThreadTS: rootTS, User: "UME", Text: "Synthetic recovered reply"}}})
	f.failHistory["C09"] = true
	f.mu.Unlock()
	imp.now = func() time.Time { return time.Now().Add(time.Hour) }
	release := failNativeSlackInsert(t, imp.store, "attachments")
	_, err = imp.Import(t.Context(), opts)
	require.Error(err)
	var count int
	require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "C09:"+rootTS).Scan(&count))
	assert.Zero(count, "a failed parent snapshot must remain eligible for recovery")
	release()
	_, err = imp.Import(t.Context(), opts)
	require.Error(err, "history remains unavailable; recovered thread debt still commits")
	require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT COUNT(*) FROM messages child WHERE child.source_message_id = ? AND EXISTS (SELECT 1 FROM messages parent WHERE parent.id = child.reply_to_message_id AND parent.source_id = child.source_id AND parent.source_message_id = ?)`), "C09:"+replyTS, "C09:"+rootTS).Scan(&count))
	assert.Equal(1, count)
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM attachments WHERE source_attachment_id = 'slack:FRECOVERY'`).Scan(&count))
	assert.Equal(1, count, "parent recovery must retry its failed media marker")
}
