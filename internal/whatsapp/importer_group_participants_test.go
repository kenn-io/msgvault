package whatsapp

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

type importProgressRecorder struct {
	NullProgress

	errors []string
}

func (p *importProgressRecorder) OnError(err error) {
	p.errors = append(p.errors, err.Error())
}

func createGroupParticipantsImportFixture(t *testing.T, setup func(*sql.DB)) string {
	t.Helper()
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "msgstore.db")
	db, err := sql.Open("sqlite3", path)
	require.NoError(err)

	_, err = db.Exec(`
		CREATE TABLE jid (
			_id INTEGER PRIMARY KEY, user TEXT, server TEXT, raw_string TEXT
		);
		CREATE TABLE chat (
			_id INTEGER PRIMARY KEY, jid_row_id INTEGER UNIQUE, hidden INTEGER,
			subject TEXT, sort_timestamp INTEGER
		);
		CREATE TABLE message (
			_id INTEGER PRIMARY KEY, chat_row_id INTEGER, from_me INTEGER,
			key_id TEXT, sender_jid_row_id INTEGER, timestamp INTEGER,
			message_type INTEGER, text_data TEXT, status INTEGER, starred INTEGER
		);
		CREATE TABLE message_media (
			message_row_id INTEGER PRIMARY KEY, mime_type TEXT, media_caption TEXT,
			file_size INTEGER, file_path TEXT, width INTEGER, height INTEGER,
			media_duration INTEGER
		);
		INSERT INTO jid VALUES
			(1, '120255501234567-987654321', 'g.us', '120255501234567-987654321@g.us'),
			(2, '120255501234567-123456789', 'g.us', '120255501234567-123456789@g.us'),
			(3, '12025550124', 's.whatsapp.net', '12025550124@s.whatsapp.net'),
			(4, '12025550125', 's.whatsapp.net', '12025550125@s.whatsapp.net'),
			(5, '12025550123', 's.whatsapp.net', '12025550123@s.whatsapp.net');
		INSERT INTO chat VALUES
			(10, 1, 0, 'Example Group One', 2000),
			(20, 2, 0, 'Example Group Two', 1000);
		INSERT INTO message VALUES
			(100, 10, 0, 'message-incoming', 3, 1700000000000, 1, 'incoming group message', 0, 0),
			(200, 20, 0, 'message-second-group', 3, 1700000001000, 1, 'second group message', 0, 0);
	`)
	require.NoError(err)
	if setup != nil {
		setup(db)
	}
	require.NoError(db.Close())
	return path
}

func TestImportGroupChatWithoutGroupParticipantsTable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	waDBPath := createGroupParticipantsImportFixture(t, nil)
	progress := &importProgressRecorder{}
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+12025550123"
	opts.DisplayName = "Example User"

	summary, err := NewImporter(st, progress).Import(context.Background(), waDBPath, opts)
	require.NoError(err)
	t.Logf("recorded import errors: %v", progress.errors)
	assert.Equal(int64(0), summary.Errors)
	assert.NotContains(strings.Join(progress.errors, "\n"), "fetch group participants for")
	assert.NotContains(strings.Join(progress.errors, "\n"), "no such table: group_participants")
	assert.Equal(int64(2), summary.MessagesAdded)

	var messageCount int
	err = st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ?`), summary.SourceID).Scan(&messageCount)
	require.NoError(err)
	assert.Equal(2, messageCount)

	for _, message := range []struct {
		keyID string
		body  string
	}{
		{keyID: "message-incoming", body: "incoming group message"},
		{keyID: "message-second-group", body: "second group message"},
	} {
		var messageID int64
		err = st.DB().QueryRow(
			st.Rebind(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`),
			summary.SourceID, message.keyID,
		).Scan(&messageID)
		require.NoError(err)

		var body string
		err = st.DB().QueryRow(
			st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id = ?`), messageID,
		).Scan(&body)
		require.NoError(err)
		assert.Equal(message.body, body)
	}

	var participantCount int
	err = st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*)
		FROM conversation_participants cp
		JOIN conversations c ON c.id = cp.conversation_id
		JOIN participants p ON p.id = cp.participant_id
		WHERE c.source_id = ?
		  AND c.source_conversation_id = ?
		  AND p.phone_number IN (?, ?)
	`), summary.SourceID, "120255501234567-987654321@g.us", "+12025550124", "+12025550123").Scan(&participantCount)
	require.NoError(err)
	assert.Equal(2, participantCount)
}

func TestImportGroupChatWithPopulatedGroupParticipants(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	waDBPath := createGroupParticipantsImportFixture(t, func(db *sql.DB) {
		_, err := db.Exec(`
			CREATE TABLE group_participants (gjid TEXT, jid TEXT, admin INTEGER);
			INSERT INTO group_participants VALUES
				('120255501234567-987654321@g.us', '12025550124@s.whatsapp.net', NULL),
				('120255501234567-987654321@g.us', '12025550125@s.whatsapp.net', 2);
		`)
		require.NoError(err)
	})
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+12025550123"
	opts.DisplayName = "Example User"

	summary, err := NewImporter(st, nil).Import(context.Background(), waDBPath, opts)
	require.NoError(err)
	assert.Equal(int64(0), summary.Errors)

	var adminRole string
	err = st.DB().QueryRow(st.Rebind(`
		SELECT cp.role
		FROM conversation_participants cp
		JOIN conversations c ON c.id = cp.conversation_id
		JOIN participants p ON p.id = cp.participant_id
		WHERE c.source_id = ?
		  AND c.source_conversation_id = ?
		  AND p.phone_number = ?
	`), summary.SourceID, "120255501234567-987654321@g.us", "+12025550125").Scan(&adminRole)
	require.NoError(err)
	assert.Equal("admin", adminRole)
}
