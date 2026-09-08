package whatsapp

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestDetectDatabaseKind(t *testing.T) {
	tests := []struct {
		name    string
		schema  string
		want    databaseKind
		wantErr string
	}{
		{
			name: "Android",
			schema: `
				CREATE TABLE message (_id INTEGER);
				CREATE TABLE jid (_id INTEGER);
				CREATE TABLE chat (_id INTEGER);
			`,
			want: databaseKindAndroid,
		},
		{
			name: "Apple",
			schema: `
				CREATE TABLE ZWAMESSAGE (Z_PK INTEGER);
				CREATE TABLE ZWACHATSESSION (Z_PK INTEGER);
			`,
			want: databaseKindApple,
		},
		{
			name:    "Unknown",
			schema:  `CREATE TABLE unrelated (id INTEGER);`,
			want:    databaseKindUnknown,
			wantErr: "expected Android message/jid/chat tables or Apple",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)

			db, err := sql.Open("sqlite3", ":memory:")
			require.NoError(err)
			defer func() { require.NoError(db.Close()) }()
			_, err = db.Exec(tt.schema)
			require.NoError(err)

			got, err := detectDatabaseKind(db)
			if tt.wantErr != "" {
				require.ErrorContains(err, tt.wantErr)
			} else {
				require.NoError(err)
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestOpenReadOnlyDatabase(t *testing.T) {
	require := require.New(t)

	missingPath := filepath.Join(t.TempDir(), "missing.sqlite")
	_, err := openReadOnlyDatabase(context.Background(), missingPath)
	require.Error(err)
	_, err = os.Stat(missingPath)
	require.ErrorIs(err, os.ErrNotExist)

	path := filepath.Join(t.TempDir(), "source # data.sqlite")
	createDSN, err := sqliteDatabaseDSN(path, "")
	require.NoError(err)
	db, err := sql.Open("sqlite3", createDSN)
	require.NoError(err)
	defer func() { require.NoError(db.Close()) }()
	var journalMode string
	require.NoError(db.QueryRow(`PRAGMA journal_mode=WAL`).Scan(&journalMode))
	require.Equal("wal", strings.ToLower(journalMode))
	_, err = db.Exec(`
		CREATE TABLE source_rows (id INTEGER PRIMARY KEY);
		INSERT INTO source_rows (id) VALUES (1);
	`)
	require.NoError(err)

	readOnly, err := openReadOnlyDatabase(context.Background(), path)
	require.NoError(err)
	defer func() { require.NoError(readOnly.Close()) }()
	var count int
	require.NoError(readOnly.QueryRow(`SELECT COUNT(*) FROM source_rows`).Scan(&count))
	assert.Equal(t, 1, count)

	_, err = readOnly.Exec(`INSERT INTO source_rows (id) VALUES (2)`)
	require.Error(err)
}

func TestImportAppleTextMessages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	chatDBPath := createAppleChatFixture(t)
	createAppleLIDFixture(t, filepath.Dir(chatDBPath))

	st := testutil.NewTestStore(t)
	importer := NewImporter(st, nil)
	opts := ImportOptions{
		Phone:       "+15555550100",
		DisplayName: "Test Owner",
		BatchSize:   2,
	}

	summary, err := importer.Import(context.Background(), chatDBPath, opts)
	require.NoError(err)
	assert.Equal(int64(3), summary.ChatsProcessed)
	assert.Equal(int64(9), summary.MessagesProcessed)
	assert.Equal(int64(4), summary.MessagesAdded)
	assert.Equal(int64(5), summary.MessagesSkipped)
	assert.Equal(int64(4), summary.Participants)
	assert.Equal(int64(1), summary.Errors)

	assertStoreCount(t, st.DB(), "messages", 4)
	assertStoreCount(t, st.DB(), "conversations", 3)
	assertStoreCount(t, st.DB(), "participants", 4)
	assertStoreCount(t, st.DB(), "message_bodies", 4)
	assertStoreCount(t, st.DB(), "message_raw", 4)

	type conversationRecord struct {
		kind  string
		title sql.NullString
	}
	conversations := func() map[string]conversationRecord {
		result := make(map[string]conversationRecord)
		rows, err := st.DB().Query(`
			SELECT source_conversation_id, conversation_type, title
			FROM conversations
		`)
		require.NoError(err)
		defer func() { require.NoError(rows.Close()) }()
		for rows.Next() {
			var sourceID, kind string
			var title sql.NullString
			require.NoError(rows.Scan(&sourceID, &kind, &title))
			result[sourceID] = conversationRecord{kind: kind, title: title}
		}
		require.NoError(rows.Err())
		return result
	}()
	assert.Equal("direct_chat", conversations["15555550101@s.whatsapp.net"].kind)
	assert.Equal("direct_chat", conversations["15555550102@s.whatsapp.net"].kind)
	assert.Equal("group_chat", conversations["120363000000000000@g.us"].kind)
	assert.Equal("Test Group", conversations["120363000000000000@g.us"].title.String)

	type messageRecord struct {
		phone    sql.NullString
		fromMe   bool
		sentAt   sql.NullTime
		bodyText string
	}
	messages := func() map[string]messageRecord {
		result := make(map[string]messageRecord)
		rows, err := st.DB().Query(`
			SELECT m.source_message_id, p.phone_number, m.is_from_me, m.sent_at,
			       COALESCE(mb.body_text, '')
			FROM messages m
			LEFT JOIN participants p ON p.id = m.sender_id
			LEFT JOIN message_bodies mb ON mb.message_id = m.id
		`)
		require.NoError(err)
		defer func() { require.NoError(rows.Close()) }()
		for rows.Next() {
			var sourceID string
			var record messageRecord
			require.NoError(rows.Scan(
				&sourceID, &record.phone, &record.fromMe,
				&record.sentAt, &record.bodyText,
			))
			result[sourceID] = record
		}
		require.NoError(rows.Err())
		return result
	}()

	assert.Equal("+15555550101", messages["direct-in"].phone.String)
	assert.False(messages["direct-in"].fromMe)
	assert.Equal("+15555550100", messages["direct-out"].phone.String)
	assert.True(messages["direct-out"].fromMe)
	assert.Equal("+15555550103", messages["group-in"].phone.String)
	assert.Equal("+15555550102", messages["lid-in"].phone.String)
	assert.Equal("group prototype text", messages["group-in"].bodyText)
	assert.Equal(
		time.Unix(appleEpochOffset+700000000, 250000000).UTC(),
		messages["direct-in"].sentAt.Time.UTC(),
	)

	var rawFormats int
	require.NoError(st.DB().QueryRow(`
		SELECT COUNT(*) FROM message_raw WHERE raw_format = 'whatsapp_apple_json'
	`).Scan(&rawFormats))
	assert.Equal(4, rawFormats)

	results, total, err := st.SearchMessages("group prototype", 0, 10)
	require.NoError(err)
	assert.Equal(int64(1), total)
	require.Len(results, 1)
	assert.Equal("group-in", results[0].SourceMessageID)

	var adminRole string
	require.NoError(st.DB().QueryRow(`
		SELECT cp.role
		FROM conversation_participants cp
		JOIN conversations c ON c.id = cp.conversation_id
		JOIN participants p ON p.id = cp.participant_id
		WHERE c.source_conversation_id = '120363000000000000@g.us'
		  AND p.phone_number = '+15555550103'
	`).Scan(&adminRole))
	assert.Equal("admin", adminRole)

	secondSummary, err := importer.Import(context.Background(), chatDBPath, opts)
	require.NoError(err)
	assert.Equal(int64(4), secondSummary.MessagesAdded)
	assertStoreCount(t, st.DB(), "messages", 4)
	assertStoreCount(t, st.DB(), "conversations", 3)
	assertStoreCount(t, st.DB(), "message_bodies", 4)
	assertStoreCount(t, st.DB(), "message_raw", 4)
}

func TestAppleMappingFallbacks(t *testing.T) {
	assert := assert.New(t)

	assert.Empty(applePhoneForJID("999999999999999@lid", nil))
	assert.Equal("999999999999999@lid", canonicalAppleJID("999999999999999@lid", nil))
	assert.False(isImportableAppleChat("status@broadcast"))
	assert.False(isImportableAppleChat("123@newsletter"))

	longText := sql.NullString{String: strings.Repeat("x", 101), Valid: true}
	assert.Len([]rune(appleMessageSnippet(longText).String), 100)
	assert.False(appleMessageTimestamp(appleTimestampValue{}).Valid)

	var timestamp appleTimestampValue
	require.NoError(t, timestamp.Scan(time.Unix(700000000, 250000000).UTC()))
	assert.Equal(
		time.Unix(appleEpochOffset+700000000, 250000000).UTC(),
		appleMessageTimestamp(timestamp).Time,
	)
}

func TestImportApplePushNames(t *testing.T) {
	need := require.New(t)
	check := assert.New(t)

	chatDBPath := createApplePushNameFixture(t)
	createAppleLIDFixture(t, filepath.Dir(chatDBPath))

	st := testutil.NewTestStore(t)
	importer := NewImporter(st, nil)
	_, err := importer.Import(context.Background(), chatDBPath, ImportOptions{
		Phone:       "+15555550100",
		DisplayName: "Test Owner",
	})
	need.NoError(err)

	names := make(map[string]string)
	rows, err := st.DB().Query(`
		SELECT phone_number, COALESCE(display_name, '')
		FROM participants
		WHERE phone_number IN ('+15555550103', '+15555550104')
	`)
	need.NoError(err)
	defer func() { need.NoError(rows.Close()) }()
	for rows.Next() {
		var phone, name string
		need.NoError(rows.Scan(&phone, &name))
		names[phone] = name
	}
	need.NoError(rows.Err())
	check.Equal("Push Roster Only", names["+15555550103"])
	check.Equal("Push Name Only", names["+15555550104"])

	var messageCount int
	need.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages`,
	).Scan(&messageCount))
	check.Equal(5, messageCount)
	var senderPhone string
	need.NoError(st.DB().QueryRow(`
		SELECT p.phone_number
		FROM messages m
		JOIN participants p ON p.id = m.sender_id
		WHERE m.source_message_id = 'push-name-only'
	`).Scan(&senderPhone))
	check.Equal("+15555550104", senderPhone)

	t.Run("message_pushname_not_used", func(t *testing.T) {
		var badNameCount int
		require.NoError(t, st.DB().QueryRow(
			`SELECT COUNT(*) FROM participants WHERE display_name = 'IAA='`,
		).Scan(&badNameCount))
		assert.Zero(t, badNameCount)
	})
}

func TestImportApplePushNameFallbacks(t *testing.T) {
	check := assert.New(t)
	need := require.New(t)
	chatDBPath := createApplePushNameFallbackFixture(t)
	createAppleLIDFixture(t, filepath.Dir(chatDBPath))

	st := testutil.NewTestStore(t)
	importer := NewImporter(st, nil)
	_, err := importer.Import(context.Background(), chatDBPath, ImportOptions{
		Phone:       "+15555550100",
		DisplayName: "Test Owner",
	})
	need.NoError(err)

	names := make(map[string]string)
	rows, err := st.DB().Query(`
		SELECT phone_number, COALESCE(display_name, '')
		FROM participants
		WHERE phone_number IN (
			'+15555550101', '+15555550103', '+15555550105',
			'+15555550106', '+15555550107', '+15555550108'
		)
	`)
	need.NoError(err)
	defer func() { need.NoError(rows.Close()) }()
	for rows.Next() {
		var phone, name string
		need.NoError(rows.Scan(&phone, &name))
		names[phone] = name
	}
	need.NoError(rows.Err())
	check.Equal("Alice Test", names["+15555550101"])
	check.Equal("Bob Test", names["+15555550103"])
	check.Empty(names["+15555550105"])
	check.Empty(names["+15555550106"])
	check.Equal("Later Legacy", names["+15555550107"])
	check.Equal("Push Direct", names["+15555550108"])

	var unmatched int
	need.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM participants WHERE phone_number = '+15555550199'`,
	).Scan(&unmatched))
	check.Zero(unmatched)

	var unresolved int
	need.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM participants WHERE display_name = 'Ignored Unresolved LID'`,
	).Scan(&unresolved))
	check.Zero(unresolved)
}

func TestImportApplePushNameErrors(t *testing.T) {
	t.Run("absent_profile_table", func(t *testing.T) {
		chatDBPath := createAppleChatFixture(t)
		createAppleLIDFixture(t, filepath.Dir(chatDBPath))

		st := testutil.NewTestStore(t)
		_, err := NewImporter(st, nil).Import(context.Background(), chatDBPath, ImportOptions{
			Phone:       "+15555550100",
			DisplayName: "Test Owner",
		})
		assert.NoError(t, err)
	})

	t.Run("malformed_profile_table", func(t *testing.T) {
		check := assert.New(t)
		need := require.New(t)
		chatDBPath := createAppleChatFixture(t)
		db, err := sql.Open("sqlite3", chatDBPath)
		need.NoError(err)
		_, err = db.Exec(`
			CREATE TABLE ZWAPROFILEPUSHNAME (
				Z_PK INTEGER PRIMARY KEY,
				ZJID TEXT
			);
		`)
		need.NoError(err)
		need.NoError(db.Close())

		st := testutil.NewTestStore(t)
		_, err = NewImporter(st, nil).Import(context.Background(), chatDBPath, ImportOptions{
			Phone:       "+15555550100",
			DisplayName: "Test Owner",
		})
		need.Error(err)
		check.ErrorContains(err, "ZWAPROFILEPUSHNAME")
	})
}

func createAppleChatFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ChatStorage.sqlite")
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = db.Exec(`
		CREATE TABLE ZWACHATSESSION (
			Z_PK INTEGER PRIMARY KEY,
			ZCONTACTJID TEXT,
			ZPARTNERNAME TEXT,
			ZSESSIONTYPE INTEGER,
			ZLASTMESSAGEDATE TIMESTAMP
		);
		CREATE TABLE ZWAGROUPMEMBER (
			Z_PK INTEGER PRIMARY KEY,
			ZCHATSESSION INTEGER,
			ZMEMBERJID TEXT,
			ZCONTACTNAME TEXT,
			ZFIRSTNAME TEXT,
			ZISADMIN INTEGER
		);
		CREATE TABLE ZWAMESSAGE (
			Z_PK INTEGER PRIMARY KEY,
			ZCHATSESSION INTEGER,
			ZGROUPMEMBER INTEGER,
			ZSTANZAID TEXT,
			ZISFROMME INTEGER,
			ZMESSAGEDATE TIMESTAMP,
			ZTEXT TEXT,
			ZMESSAGETYPE INTEGER,
			ZFROMJID TEXT
		);

		INSERT INTO ZWACHATSESSION VALUES
			(1, '15555550101@s.whatsapp.net', 'Alice Test', 0, 700000002),
			(2, '120363000000000000@g.us', 'Test Group', 1, 700000010),
			(3, '999999999999999@lid', 'LID Test', 0, 700000020),
			(4, 'status@broadcast', 'Status', 3, 700000030);

		INSERT INTO ZWAGROUPMEMBER VALUES
			(10, 2, '888888888888888@lid', 'Bob Test', 'Bob', 1);

		INSERT INTO ZWAMESSAGE VALUES
			(1, 1, NULL, 'direct-in', 0, 700000000.25, 'direct prototype text', 0, '15555550101@s.whatsapp.net'),
			(2, 1, NULL, 'direct-out', 1, 700000001, 'outbound prototype text', 0, ''),
			(3, 2, 10, 'group-in', 0, 700000002, 'group prototype text', 0, '120363000000000000@g.us'),
			(4, 2, 10, 'group-image', 0, 700000003, 'image caption', 1, '120363000000000000@g.us'),
			(5, 3, NULL, 'lid-in', 0, 700000004, 'lid prototype text', 0, '999999999999999@lid'),
			(6, 4, NULL, 'status-text', 0, 700000005, 'ignored status text', 0, 'status@broadcast'),
			(7, 1, NULL, 'duplicate-text', 0, 700000006, 'first duplicate', 0, '15555550101@s.whatsapp.net'),
			(8, 2, 10, 'duplicate-text', 0, 700000007, 'second duplicate', 0, '120363000000000000@g.us'),
			(9, 1, NULL, '   ', 0, 700000008, 'missing stanza', 0, '15555550101@s.whatsapp.net'),
			(10, 1, NULL, 'empty-text', 0, 700000009, '   ', 0, '15555550101@s.whatsapp.net');
	`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	return path
}

func createAppleLIDFixture(t *testing.T, dir string) {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(dir, "LID.sqlite"))
	require.NoError(t, err)
	_, err = db.Exec(`
		CREATE TABLE ZWAZACCOUNT (
			ZIDENTIFIER TEXT,
			ZPHONENUMBER TEXT
		);
		INSERT INTO ZWAZACCOUNT VALUES
			('999999999999999@lid', '15555550102'),
			('888888888888888@lid', '15555550103');
	`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
}

func createApplePushNameFixture(t *testing.T) string {
	t.Helper()
	path := createAppleChatFixture(t)
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = db.Exec(`
		ALTER TABLE ZWAMESSAGE ADD COLUMN ZPUSHNAME TEXT;
		UPDATE ZWAGROUPMEMBER
		SET ZCONTACTNAME = '', ZFIRSTNAME = ''
		WHERE Z_PK = 10;
		INSERT INTO ZWAGROUPMEMBER
			(Z_PK, ZCHATSESSION, ZMEMBERJID, ZCONTACTNAME, ZFIRSTNAME, ZISADMIN)
		VALUES (11, NULL, '15555550104@s.whatsapp.net', '', '', 0);
		INSERT INTO ZWAMESSAGE
			(Z_PK, ZCHATSESSION, ZGROUPMEMBER, ZSTANZAID, ZISFROMME,
			 ZMESSAGEDATE, ZTEXT, ZMESSAGETYPE, ZFROMJID, ZPUSHNAME)
		VALUES
			(11, 2, 11, 'push-name-only', 0, 700000011,
			 'sender profile text', 0, '120363000000000000@g.us', 'IAA=');
		CREATE TABLE ZWAPROFILEPUSHNAME (
			Z_PK INTEGER PRIMARY KEY,
			Z_ENT INTEGER,
			Z_OPT INTEGER,
			ZJID TEXT,
			ZPUSHNAME TEXT
		);
		INSERT INTO ZWAPROFILEPUSHNAME (Z_PK, Z_ENT, Z_OPT, ZJID, ZPUSHNAME)
		VALUES
			(1, 1, 1, '888888888888888@lid', 'Push Roster Only'),
			(2, 1, 1, '15555550104@s.whatsapp.net', 'Push Name Only'),
			(3, 1, 1, '888888888888888@lid', 'Later Roster Name');
	`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	return path
}

func createApplePushNameFallbackFixture(t *testing.T) string {
	t.Helper()
	path := createAppleChatFixture(t)
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO ZWACHATSESSION
			(Z_PK, ZCONTACTJID, ZPARTNERNAME, ZSESSIONTYPE, ZLASTMESSAGEDATE)
		VALUES
			(5, '120363000000000001@g.us', 'Future Group', 1, 700000100),
			(6, '15555550107@s.whatsapp.net', 'Later Legacy', 0, 700000050),
			(7, '15555550108@s.whatsapp.net', '', 0, 700000040);
		INSERT INTO ZWAGROUPMEMBER
			(Z_PK, ZCHATSESSION, ZMEMBERJID, ZCONTACTNAME, ZFIRSTNAME, ZISADMIN)
		VALUES
			(11, 2, '15555550105@s.whatsapp.net', '', '', 0),
			(12, 2, '15555550106@s.whatsapp.net', '', '', 0),
			(13, 5, '15555550107@s.whatsapp.net', '', '', 0),
			(14, 2, '777777777777777@lid', '', '', 0);
		CREATE TABLE ZWAPROFILEPUSHNAME (
			Z_PK INTEGER PRIMARY KEY,
			Z_ENT INTEGER,
			Z_OPT INTEGER,
			ZJID TEXT,
			ZPUSHNAME TEXT
		);
		INSERT INTO ZWAPROFILEPUSHNAME (Z_PK, Z_ENT, Z_OPT, ZJID, ZPUSHNAME)
		VALUES
			(1, 1, 1, '888888888888888@lid', 'Profile Bob'),
			(2, 1, 1, '15555550105@s.whatsapp.net', '   '),
			(3, 1, 1, '15555550106@s.whatsapp.net', ''),
			(4, 1, 1, '15555550107@s.whatsapp.net', 'Push Early'),
			(5, 1, 1, '15555550101@s.whatsapp.net', 'Ignored Direct'),
			(6, 1, 1, '15555550199@s.whatsapp.net', 'Unmatched'),
			(7, 1, 1, '15555550108@s.whatsapp.net', 'Push Direct'),
			(8, 1, 1, '777777777777777@lid', 'Ignored Unresolved LID');
	`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	return path
}

func assertStoreCount(t *testing.T, db *sql.DB, table string, want int) {
	t.Helper()
	var got int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM "+table).Scan(&got))
	assert.Equal(t, want, got, table)
}
