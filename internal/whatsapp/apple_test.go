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

	path := filepath.Join(t.TempDir(), "source ? data.sqlite")
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

func assertStoreCount(t *testing.T, db *sql.DB, table string, want int) {
	t.Helper()
	var got int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM "+table).Scan(&got))
	assert.Equal(t, want, got, table)
}
