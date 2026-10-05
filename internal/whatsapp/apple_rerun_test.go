package whatsapp

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func execAppleFixture(t *testing.T, path, statements string) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = db.Exec(statements)
	require.NoError(t, err)
	require.NoError(t, db.Close())
}

var appleRerunSentinel = time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)

// markAppleMessagesUnwritten stamps every message so a later rewrite shows up
// as a changed last_modified value.
func markAppleMessagesUnwritten(t *testing.T, st *store.Store) {
	t.Helper()
	_, err := st.DB().Exec(st.Rebind(`UPDATE messages SET last_modified = ?`), appleRerunSentinel)
	require.NoError(t, err)
}

// rewrittenAppleMessages lists the source IDs whose last_modified moved off
// the sentinel since markAppleMessagesUnwritten.
func rewrittenAppleMessages(t *testing.T, st *store.Store) []string {
	t.Helper()
	rows, err := st.DB().Query(`SELECT source_message_id, last_modified FROM messages ORDER BY source_message_id`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var rewritten []string
	for rows.Next() {
		var id string
		var modified time.Time
		require.NoError(t, rows.Scan(&id, &modified))
		if !modified.Equal(appleRerunSentinel) {
			rewritten = append(rewritten, id)
		}
	}
	require.NoError(t, rows.Err())
	return rewritten
}

func appleBodyText(t *testing.T, st *store.Store, sourceMessageID string) string {
	t.Helper()
	var body string
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT COALESCE(mb.body_text, '')
		FROM messages m
		JOIN message_bodies mb ON mb.message_id = m.id
		WHERE m.source_message_id = ?
	`), sourceMessageID).Scan(&body))
	return body
}

func appleSearchIDs(t *testing.T, st *store.Store, query string) []string {
	t.Helper()
	results, _, err := st.SearchMessages(query, 0, 50)
	require.NoError(t, err)
	ids := make([]string, 0, len(results))
	for _, result := range results {
		ids = append(ids, result.SourceMessageID)
	}
	return ids
}

// appleSearchNamesSender reports whether a message's search document carries
// the sender phone, as an import writes it.
func appleSearchNamesSender(t *testing.T, st *store.Store, sourceMessageID, body, phone string) bool {
	t.Helper()
	var id int64
	require.NoError(t, st.DB().QueryRow(
		st.Rebind(`SELECT id FROM messages WHERE source_message_id = ?`), sourceMessageID,
	).Scan(&id))
	matches, err := st.MessageContentMatchesContext(
		context.Background(), id, sql.NullString{String: body, Valid: true},
		store.FTSDoc{Body: body, FromAddr: phone},
	)
	require.NoError(t, err)
	return matches
}

func appleTestOptions() ImportOptions {
	return ImportOptions{Phone: "+15555550100", DisplayName: "Test Owner", BatchSize: 2}
}

func TestImportAppleRerunSkipsUnchangedMessages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	chatDBPath := createAppleChatFixture(t)
	createAppleLIDFixture(t, filepath.Dir(chatDBPath))
	// A sub-microsecond fraction must not read as a change on PostgreSQL.
	execAppleFixture(t, chatDBPath, `UPDATE ZWAMESSAGE SET ZMESSAGEDATE = 700000001.1234567 WHERE Z_PK = 2`)

	st := testutil.NewTestStore(t)
	importer := NewImporter(st, nil)
	first, err := importer.Import(context.Background(), chatDBPath, appleTestOptions())
	require.NoError(err)
	require.Equal(int64(4), first.MessagesAdded)
	markAppleMessagesUnwritten(t, st)

	second, err := importer.Import(context.Background(), chatDBPath, appleTestOptions())
	require.NoError(err)
	assert.Equal(int64(0), second.MessagesAdded)
	assert.Equal(second.MessagesProcessed, second.MessagesSkipped)
	assert.Empty(rewrittenAppleMessages(t, st))
	assertStoreCount(t, st.DB(), "messages", 4)
}

func TestImportAppleRerunUpdatesEditedMessage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	prefix := strings.Repeat("a", 100)
	chatDBPath := createAppleChatFixture(t)
	createAppleLIDFixture(t, filepath.Dir(chatDBPath))
	execAppleFixture(t, chatDBPath, `UPDATE ZWAMESSAGE SET ZTEXT = '`+prefix+` originaltail' WHERE Z_PK = 1`)

	st := testutil.NewTestStore(t)
	importer := NewImporter(st, nil)
	first, err := importer.Import(context.Background(), chatDBPath, appleTestOptions())
	require.NoError(err)
	labelID, err := st.EnsureLabel(first.SourceID, "kept", "Kept", "user")
	require.NoError(err)
	var messageID int64
	require.NoError(st.DB().QueryRow(
		`SELECT id FROM messages WHERE source_message_id = 'direct-in'`,
	).Scan(&messageID))
	require.NoError(st.AddMessageLabels(messageID, []int64{labelID}))
	markAppleMessagesUnwritten(t, st)

	// The edit lands past the snippet, and the message is years old.
	execAppleFixture(t, chatDBPath, `UPDATE ZWAMESSAGE SET ZTEXT = '`+prefix+` editedtail' WHERE Z_PK = 1`)
	second, err := importer.Import(context.Background(), chatDBPath, appleTestOptions())
	require.NoError(err)
	assert.Equal(int64(1), second.MessagesAdded)
	assert.Equal([]string{"direct-in"}, rewrittenAppleMessages(t, st))
	assert.Equal(prefix+" editedtail", appleBodyText(t, st, "direct-in"))
	raw, err := st.GetMessageRaw(messageID)
	require.NoError(err)
	assert.Contains(string(raw), "editedtail")
	assert.Equal([]string{"direct-in"}, appleSearchIDs(t, st, "editedtail"))
	assert.Empty(appleSearchIDs(t, st, "originaltail"))
	var labels int
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT COUNT(*) FROM message_labels WHERE message_id = ? AND label_id = ?`), messageID, labelID,
	).Scan(&labels))
	assert.Equal(1, labels)
}

func TestImportAppleRerunReattributesLIDSenders(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	chatDBPath := createAppleChatFixture(t)
	st := testutil.NewTestStore(t)
	importer := NewImporter(st, nil)
	_, err := importer.Import(context.Background(), chatDBPath, appleTestOptions())
	require.NoError(err)
	markAppleMessagesUnwritten(t, st)

	createAppleLIDFixture(t, filepath.Dir(chatDBPath))
	second, err := importer.Import(context.Background(), chatDBPath, appleTestOptions())
	require.NoError(err)
	assert.Equal(int64(2), second.MessagesAdded)
	assert.Equal([]string{"group-in", "lid-in"}, rewrittenAppleMessages(t, st))

	attribution := func(sourceMessageID string) (string, string) {
		var conversation string
		var phone sql.NullString
		require.NoError(st.DB().QueryRow(st.Rebind(`
			SELECT c.source_conversation_id, p.phone_number
			FROM messages m
			JOIN conversations c ON c.id = m.conversation_id
			LEFT JOIN participants p ON p.id = m.sender_id
			WHERE m.source_message_id = ?
		`), sourceMessageID).Scan(&conversation, &phone))
		return conversation, phone.String
	}
	conversation, phone := attribution("lid-in")
	assert.Equal("15555550102@s.whatsapp.net", conversation)
	assert.Equal("+15555550102", phone)
	conversation, phone = attribution("group-in")
	assert.Equal("120363000000000000@g.us", conversation)
	assert.Equal("+15555550103", phone)
	assert.True(appleSearchNamesSender(t, st, "group-in", "group prototype text", "+15555550103"))

	third, err := importer.Import(context.Background(), chatDBPath, appleTestOptions())
	require.NoError(err)
	assert.Equal(int64(0), third.MessagesAdded)
}

// installAppleRawWriteFailure makes the real database reject raw payload
// writes until the returned release function removes the trigger.
func installAppleRawWriteFailure(t *testing.T, st *store.Store) func() {
	t.Helper()
	require := require.New(t)

	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(`CREATE FUNCTION fail_apple_raw_write()
			RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				RAISE EXCEPTION 'forced raw write failure';
			END;
			$$`)
		require.NoError(err, "create PostgreSQL failure function")
		_, err = st.DB().Exec(`CREATE TRIGGER fail_apple_raw_write
			BEFORE INSERT ON message_raw
			FOR EACH ROW EXECUTE FUNCTION fail_apple_raw_write()`)
		require.NoError(err, "create PostgreSQL failure trigger")
		release := func() {
			_, err := st.DB().Exec(`DROP TRIGGER IF EXISTS fail_apple_raw_write ON message_raw`)
			require.NoError(err, "drop PostgreSQL failure trigger")
			_, err = st.DB().Exec(`DROP FUNCTION IF EXISTS fail_apple_raw_write()`)
			require.NoError(err, "drop PostgreSQL failure function")
		}
		t.Cleanup(release)
		return release
	}

	_, err := st.DB().Exec(`CREATE TRIGGER fail_apple_raw_write
		BEFORE INSERT ON message_raw
		FOR EACH ROW BEGIN
			SELECT RAISE(ABORT, 'forced raw write failure');
		END`)
	require.NoError(err, "create SQLite failure trigger")
	release := func() {
		_, err := st.DB().Exec(`DROP TRIGGER IF EXISTS fail_apple_raw_write`)
		require.NoError(err, "drop SQLite failure trigger")
	}
	t.Cleanup(release)
	return release
}

func TestImportAppleFailedWriteLeavesNoPartialMessage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	chatDBPath := createAppleChatFixture(t)
	createAppleLIDFixture(t, filepath.Dir(chatDBPath))
	st := testutil.NewTestStore(t)
	importer := NewImporter(st, nil)
	_, err := importer.Import(context.Background(), chatDBPath, appleTestOptions())
	require.NoError(err)

	// A new message whose raw write fails leaves no message, body or raw row.
	execAppleFixture(t, chatDBPath, `
		INSERT INTO ZWAMESSAGE VALUES
			(20, 1, NULL, 'direct-new', 0, 700000020, 'new prototype text', 0, '15555550101@s.whatsapp.net');
	`)
	release := installAppleRawWriteFailure(t, st)
	_, err = importer.Import(context.Background(), chatDBPath, appleTestOptions())
	require.ErrorContains(err, "persist Apple message")
	var newRows int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = 'direct-new'`,
	).Scan(&newRows))
	assert.Zero(newRows)
	assertStoreCount(t, st.DB(), "message_bodies", 4)
	assertStoreCount(t, st.DB(), "message_raw", 4)
	release()
	inserted, err := importer.Import(context.Background(), chatDBPath, appleTestOptions())
	require.NoError(err)
	assert.Equal(int64(1), inserted.MessagesAdded)
	assert.Equal("new prototype text", appleBodyText(t, st, "direct-new"))

	// An edit whose raw write fails keeps the previous version intact.
	execAppleFixture(t, chatDBPath, `UPDATE ZWAMESSAGE SET ZTEXT = 'edited outbound text' WHERE Z_PK = 2`)
	release = installAppleRawWriteFailure(t, st)
	_, err = importer.Import(context.Background(), chatDBPath, appleTestOptions())
	require.ErrorContains(err, "persist Apple message")
	var snippet string
	require.NoError(st.DB().QueryRow(
		`SELECT snippet FROM messages WHERE source_message_id = 'direct-out'`,
	).Scan(&snippet))
	assert.Equal("outbound prototype text", snippet)
	assert.Equal("outbound prototype text", appleBodyText(t, st, "direct-out"))
	assert.Equal([]string{"direct-out"}, appleSearchIDs(t, st, "outbound"))
	release()
	edited, err := importer.Import(context.Background(), chatDBPath, appleTestOptions())
	require.NoError(err)
	assert.Equal(int64(1), edited.MessagesAdded)
	assert.Equal("edited outbound text", appleBodyText(t, st, "direct-out"))
}

func TestImportAppleRerunRepairsPartialWrites(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	chatDBPath := createAppleChatFixture(t)
	createAppleLIDFixture(t, filepath.Dir(chatDBPath))
	st := testutil.NewTestStore(t)
	importer := NewImporter(st, nil)
	_, err := importer.Import(context.Background(), chatDBPath, appleTestOptions())
	require.NoError(err)

	// Earlier releases wrote the search entry and body in separate commits, so
	// an interrupted run could leave either one behind the rest of the message.
	ids := make(map[string]int64)
	for _, sourceID := range []string{"group-in", "direct-in", "lid-in"} {
		var id int64
		require.NoError(st.DB().QueryRow(
			st.Rebind(`SELECT id FROM messages WHERE source_message_id = ?`), sourceID,
		).Scan(&id))
		ids[sourceID] = id
	}
	require.NoError(st.UpsertFTS(ids["group-in"], "", "group prototype text", "", "", ""))
	require.NoError(st.UpsertMessageBody(
		ids["direct-in"], sql.NullString{String: "stale body", Valid: true}, sql.NullString{},
	))
	_, err = st.DB().Exec(
		st.Rebind(`UPDATE message_raw SET raw_data = ?, compression = 'zlib' WHERE message_id = ?`),
		[]byte("not zlib"), ids["lid-in"],
	)
	require.NoError(err)
	require.False(appleSearchNamesSender(t, st, "group-in", "group prototype text", "+15555550103"))
	markAppleMessagesUnwritten(t, st)

	summary, err := importer.Import(context.Background(), chatDBPath, appleTestOptions())
	require.NoError(err)
	assert.Equal(int64(3), summary.MessagesAdded)
	assert.Equal([]string{"direct-in", "group-in", "lid-in"}, rewrittenAppleMessages(t, st))
	raw, err := st.GetMessageRaw(ids["lid-in"])
	require.NoError(err)
	assert.Contains(string(raw), "lid prototype text")
	assert.True(appleSearchNamesSender(t, st, "group-in", "group prototype text", "+15555550103"))
	assert.Equal("direct prototype text", appleBodyText(t, st, "direct-in"))

	again, err := importer.Import(context.Background(), chatDBPath, appleTestOptions())
	require.NoError(err)
	assert.Equal(int64(0), again.MessagesAdded)
}

func TestImportAppleRerunFollowsChatMove(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	chatDBPath := createAppleChatFixture(t)
	createAppleLIDFixture(t, filepath.Dir(chatDBPath))
	st := testutil.NewTestStore(t)
	importer := NewImporter(st, nil)
	_, err := importer.Import(context.Background(), chatDBPath, appleTestOptions())
	require.NoError(err)

	// Chat 3 is scanned before chat 1, so the moved row is seen there first.
	execAppleFixture(t, chatDBPath, `UPDATE ZWAMESSAGE SET ZCHATSESSION = 3 WHERE Z_PK = 2`)
	moved, err := importer.Import(context.Background(), chatDBPath, appleTestOptions())
	require.NoError(err)
	assert.Equal(int64(1), moved.MessagesAdded)

	var conversation string
	require.NoError(st.DB().QueryRow(`
		SELECT c.source_conversation_id
		FROM messages m JOIN conversations c ON c.id = m.conversation_id
		WHERE m.source_message_id = 'direct-out'
	`).Scan(&conversation))
	assert.Equal("15555550102@s.whatsapp.net", conversation)

	again, err := importer.Import(context.Background(), chatDBPath, appleTestOptions())
	require.NoError(err)
	assert.Equal(int64(0), again.MessagesAdded)
}

func TestImportAppleRerunSkipsIdentityAttributedMessage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	chatDBPath := createAppleChatFixture(t)
	createAppleLIDFixture(t, filepath.Dir(chatDBPath))
	st := testutil.NewTestStore(t)
	importer := NewImporter(st, nil)
	first, err := importer.Import(context.Background(), chatDBPath, appleTestOptions())
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(first.SourceID, "+15555550101", "manual"))
	var fromMe bool
	require.NoError(st.DB().QueryRow(
		`SELECT is_from_me FROM messages WHERE source_message_id = 'direct-in'`,
	).Scan(&fromMe))
	require.True(fromMe, "identity confirmation marks the incoming message as the account's")

	second, err := importer.Import(context.Background(), chatDBPath, appleTestOptions())
	require.NoError(err)
	assert.Equal(int64(0), second.MessagesAdded)
}

func TestImportAppleRerunRefreshesNamesWithoutWrites(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	chatDBPath := createApplePushNameFixture(t)
	createAppleLIDFixture(t, filepath.Dir(chatDBPath))
	execAppleFixture(t, chatDBPath, `DELETE FROM ZWAPROFILEPUSHNAME WHERE Z_PK = 2`)
	st := testutil.NewTestStore(t)
	importer := NewImporter(st, nil)
	_, err := importer.Import(context.Background(), chatDBPath, appleTestOptions())
	require.NoError(err)
	markAppleMessagesUnwritten(t, st)

	execAppleFixture(t, chatDBPath, `
		INSERT INTO ZWAPROFILEPUSHNAME (Z_PK, Z_ENT, Z_OPT, ZJID, ZPUSHNAME)
		VALUES (2, 1, 1, '15555550104@s.whatsapp.net', 'Push Name Only')
	`)
	summary, err := importer.Import(context.Background(), chatDBPath, appleTestOptions())
	require.NoError(err)
	assert.Equal(int64(0), summary.MessagesAdded)
	assert.Empty(rewrittenAppleMessages(t, st))
	var name string
	require.NoError(st.DB().QueryRow(
		`SELECT COALESCE(display_name, '') FROM participants WHERE phone_number = '+15555550104'`,
	).Scan(&name))
	assert.Equal("Push Name Only", name)
}

func TestImportAppleLimitSkipsUnchangedPrefix(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	chatDBPath := createAppleChatFixture(t)
	createAppleLIDFixture(t, filepath.Dir(chatDBPath))
	execAppleFixture(t, chatDBPath, `
		INSERT INTO ZWAMESSAGE VALUES
			(11, 1, NULL, 'extra-1', 0, 700000011, 'extra one', 0, '15555550101@s.whatsapp.net'),
			(12, 1, NULL, 'extra-2', 0, 700000012, 'extra two', 0, '15555550101@s.whatsapp.net'),
			(13, 1, NULL, 'extra-3', 0, 700000013, 'extra three', 0, '15555550101@s.whatsapp.net'),
			(14, 1, NULL, 'late-1', 0, 700000014, 'late one', 0, '15555550101@s.whatsapp.net'),
			(15, 1, NULL, 'late-2', 0, 700000015, 'late two', 0, '15555550101@s.whatsapp.net');
	`)
	st := testutil.NewTestStore(t)
	importer := NewImporter(st, nil)
	limited := appleTestOptions()
	limited.Limit = 1

	// A limited first import reaches a different message on each run.
	for run := 1; run <= 2; run++ {
		summary, err := importer.Import(context.Background(), chatDBPath, limited)
		require.NoError(err)
		assert.Equal(int64(1), summary.MessagesAdded)
		assertStoreCount(t, st.DB(), "messages", run)
	}
	_, err := importer.Import(context.Background(), chatDBPath, appleTestOptions())
	require.NoError(err)
	assertStoreCount(t, st.DB(), "messages", 9)

	execAppleFixture(t, chatDBPath, `
		UPDATE ZWAMESSAGE SET ZTEXT = 'late one edited' WHERE Z_PK = 14;
		UPDATE ZWAMESSAGE SET ZTEXT = 'late two edited' WHERE Z_PK = 15;
	`)
	first, err := importer.Import(context.Background(), chatDBPath, limited)
	require.NoError(err)
	assert.Equal(int64(1), first.MessagesAdded)
	assert.GreaterOrEqual(first.MessagesSkipped, int64(7))
	assert.Equal("late one edited", appleBodyText(t, st, "late-1"))
	assert.Equal("late two", appleBodyText(t, st, "late-2"))

	second, err := importer.Import(context.Background(), chatDBPath, limited)
	require.NoError(err)
	assert.Equal(int64(1), second.MessagesAdded)
	assert.Equal("late two edited", appleBodyText(t, st, "late-2"))

	final, err := importer.Import(context.Background(), chatDBPath, appleTestOptions())
	require.NoError(err)
	assert.Equal(int64(0), final.MessagesAdded)
}
