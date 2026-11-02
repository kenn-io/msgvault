package signalandroid

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

// Synthetic identities. The phone numbers use the reserved 555-01xx range.
const (
	ownPhone   = "+15555550100"
	alicePhone = "+15555550101"
	carolPhone = "+15555550103"
	ownACI     = "0f0f0f0f-0000-4000-8000-000000000001"
	aliceACI   = "0f0f0f0f-0000-4000-8000-000000000002"
	bobACI     = "0f0f0f0f-0000-4000-8000-000000000003"
	groupHex   = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	// groupB64 is groupHex as unpadded base64url.
	groupB64 = "ABEiM0RVZneImaq7zN3u_wARIjNEVWZ3iJmqu8zd7v8"

	typeIncoming     = 20 | 0x200000 | 0x800000
	typeOutgoing     = 23 | 0x200000 | 0x800000
	typeSMSIncoming  = 20
	typeCallIncoming = 1
	typeGroupUpdate  = 20 | 0x10000 | 0x80000 | 0x200000 | 0x800000

	tAlice1  = int64(1_700_000_000_000)
	tReply   = int64(1_700_000_060_000)
	tEditOld = int64(1_700_000_120_000)
	tEditNew = int64(1_700_000_180_000)
	// tQuoteDraft lies between the draft and its edit.
	tQuoteDraft = int64(1_700_000_150_000)
	tGroup      = int64(1_700_000_240_000)
	tCarol      = int64(1_700_000_300_000)
	tSMS        = int64(1_700_000_360_000)
)

// fixtureLayout selects the column names of a Signal database version.
type fixtureLayout struct {
	aci, e164, attachment, attachmentMessage, attachmentType string
}

var (
	// layoutCurrent matches database version 215 and newer.
	layoutCurrent = fixtureLayout{"aci", "e164", "attachment", "message_id", "content_type"}
	// layoutV185 matches database versions 185 to 199.
	layoutV185 = fixtureLayout{"uuid", "phone", "part", "mid", "ct"}
)

// writeSignalFixture writes a small Signal Android database and an
// attachment file into dir. offset shifts every local row ID, as restoring a
// backup on another phone can.
func writeSignalFixture(t *testing.T, dir string, layout fixtureLayout, offset int64) string {
	t.Helper()
	path := filepath.Join(dir, "database.sqlite")
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()

	id := func(n int64) int64 { return n + offset }
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE recipient (_id INTEGER PRIMARY KEY, type INTEGER DEFAULT 0,
			%s TEXT, %s TEXT, group_id TEXT, distribution_list_id INTEGER,
			system_joined_name TEXT, profile_joined_name TEXT)`, layout.e164, layout.aci),
		`CREATE TABLE thread (_id INTEGER PRIMARY KEY, recipient_id INTEGER)`,
		`CREATE TABLE message (_id INTEGER PRIMARY KEY, date_sent INTEGER NOT NULL,
			date_received INTEGER NOT NULL, thread_id INTEGER NOT NULL,
			from_recipient_id INTEGER NOT NULL, to_recipient_id INTEGER NOT NULL,
			type INTEGER NOT NULL, body TEXT, quote_id INTEGER DEFAULT 0,
			quote_author INTEGER DEFAULT 0, remote_deleted INTEGER DEFAULT 0,
			story_type INTEGER DEFAULT 0, parent_story_id INTEGER DEFAULT 0,
			scheduled_date INTEGER DEFAULT -1, view_once INTEGER DEFAULT 0,
			latest_revision_id INTEGER, original_message_id INTEGER,
			revision_number INTEGER DEFAULT 0)`,
		fmt.Sprintf(`CREATE TABLE %s (_id INTEGER PRIMARY KEY, %s INTEGER, %s TEXT,
			file_name TEXT, data_size INTEGER, width INTEGER DEFAULT 0, height INTEGER DEFAULT 0,
			caption TEXT, voice_note INTEGER DEFAULT 0, video_gif INTEGER DEFAULT 0,
			quote INTEGER DEFAULT 0, sticker_pack_id TEXT, display_order INTEGER DEFAULT 0)`,
			layout.attachment, layout.attachmentMessage, layout.attachmentType),
		`CREATE TABLE groups (_id INTEGER PRIMARY KEY, group_id TEXT, recipient_id INTEGER, title TEXT)`,
		`CREATE TABLE group_membership (_id INTEGER PRIMARY KEY, group_id TEXT, recipient_id INTEGER)`,
		`CREATE TABLE reaction (_id INTEGER PRIMARY KEY, message_id INTEGER NOT NULL,
			author_id INTEGER NOT NULL, emoji TEXT NOT NULL, date_sent INTEGER NOT NULL,
			date_received INTEGER NOT NULL)`,
		`CREATE TABLE mention (_id INTEGER PRIMARY KEY, thread_id INTEGER, message_id INTEGER,
			recipient_id INTEGER, range_start INTEGER, range_length INTEGER)`,
	}
	for _, stmt := range stmts {
		_, err := db.Exec(stmt)
		require.NoError(t, err, stmt)
	}

	exec := func(query string, args ...any) {
		t.Helper()
		_, err := db.Exec(query, args...)
		require.NoError(t, err, query)
	}
	recipientSQL := fmt.Sprintf(`INSERT INTO recipient (_id, %s, %s, group_id, system_joined_name, profile_joined_name)
		VALUES (?, ?, ?, ?, ?, ?)`, layout.e164, layout.aci)
	exec(recipientSQL, id(1), ownPhone, ownACI, nil, nil, "Owner")
	exec(recipientSQL, id(2), alicePhone, aliceACI, nil, "Alice Example", nil)
	exec(recipientSQL, id(3), nil, bobACI, nil, nil, "Bob Example")
	exec(recipientSQL, id(4), carolPhone, nil, nil, "Carol Example", nil)
	exec(recipientSQL, id(5), nil, nil, groupV2IDPrefix+groupHex, nil, nil)
	exec(recipientSQL, id(6), nil, nil, "__textsecure_group__!00ff", nil, nil)

	exec(`INSERT INTO groups (group_id, recipient_id, title) VALUES (?, ?, ?), (?, ?, ?)`,
		groupV2IDPrefix+groupHex, id(5), "Example Group", "__textsecure_group__!00ff", id(6), "Old Group")
	exec(`INSERT INTO group_membership (group_id, recipient_id) VALUES (?, ?), (?, ?), (?, ?)`,
		groupV2IDPrefix+groupHex, id(1), groupV2IDPrefix+groupHex, id(2), groupV2IDPrefix+groupHex, id(3))

	exec(`INSERT INTO thread (_id, recipient_id) VALUES (?, ?), (?, ?), (?, ?), (?, ?)`,
		id(10), id(2), id(11), id(5), id(12), id(4), id(13), id(6))

	msgSQL := `INSERT INTO message (_id, date_sent, date_received, thread_id, from_recipient_id,
		to_recipient_id, type, body, quote_id, quote_author, latest_revision_id, original_message_id,
		revision_number) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	// Direct chat with Alice: a message, a reply quoting it, and an edit.
	exec(msgSQL, id(100), tAlice1, tAlice1+1, id(10), id(2), id(1), typeIncoming, "Hello from Alice", 0, 0, nil, nil, 0)
	exec(msgSQL, id(101), tReply, tReply+1, id(10), id(1), id(2), typeOutgoing, "Replying", tAlice1, id(2), nil, nil, 0)
	exec(msgSQL, id(102), tEditOld, tEditOld+1, id(10), id(1), id(2), typeOutgoing, "Frist draft", 0, 0, id(106), nil, 0)
	// Alice quotes the draft before it is edited, so her reply's row
	// precedes the draft's newest revision.
	exec(msgSQL, id(103), tQuoteDraft, tQuoteDraft+1, id(10), id(2), id(1), typeIncoming, "Nice draft", tEditOld, id(1), nil, nil, 0)
	exec(msgSQL, id(106), tEditNew, tEditNew+1, id(10), id(1), id(2), typeOutgoing, "First draft", 0, 0, nil, id(102), 1)
	exec(msgSQL, id(104), tSMS, tSMS+1, id(10), id(2), id(1), typeSMSIncoming, "Plain SMS", 0, 0, nil, nil, 0)
	exec(msgSQL, id(105), tSMS+1, tSMS+2, id(10), id(2), id(1), typeCallIncoming, nil, 0, 0, nil, nil, 0)
	// Group chat: a message from Bob mentioning Alice, with a photo.
	exec(msgSQL, id(200), tGroup, tGroup+1, id(11), id(3), id(5), typeIncoming, "Hi \uFFFC, look", 0, 0, nil, nil, 0)
	exec(msgSQL, id(201), tGroup+5, tGroup+6, id(11), id(1), id(5), typeGroupUpdate, nil, 0, 0, nil, nil, 0)
	// Contact known only by number.
	exec(msgSQL, id(300), tCarol, tCarol+1, id(12), id(4), id(1), typeIncoming, "Carol here", 0, 0, nil, nil, 0)
	// Legacy group, not imported.
	exec(msgSQL, id(400), tCarol+10, tCarol+11, id(13), id(4), id(6), typeIncoming, "Old group text", 0, 0, nil, nil, 0)

	exec(`INSERT INTO mention (thread_id, message_id, recipient_id, range_start, range_length) VALUES (?, ?, ?, 3, 1)`,
		id(11), id(200), id(2))
	exec(`INSERT INTO reaction (message_id, author_id, emoji, date_sent, date_received) VALUES (?, ?, ?, ?, ?), (?, ?, ?, ?, ?)`,
		id(200), id(2), "\U0001F44D", tGroup+100, tGroup+101,
		id(106), id(2), "\u2764", tEditNew+100, tEditNew+101)

	attachmentSQL := fmt.Sprintf(`INSERT INTO %s (_id, %s, %s, file_name, data_size, width, height, caption, quote)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, layout.attachment, layout.attachmentMessage, layout.attachmentType)
	exec(attachmentSQL, id(500), id(200), "image/jpeg", nil, 11, 640, 480, "Beach", 0)
	exec(attachmentSQL, id(501), id(101), "image/jpeg", nil, 5, 0, 0, nil, 1)
	exec(attachmentSQL, id(502), id(300), "application/pdf", "notes.pdf", 9, 0, 0, nil, 0)

	// Only the photo's file is present; the PDF was never downloaded.
	require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("Attachment_%d_-1.bin", id(500))),
		[]byte("photo bytes"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("Attachment_%d_-1.sbf", id(500))),
		[]byte("frame"), 0o600))
	return path
}

// writeTablesFixture writes a SQLite file containing only the given DDL.
func writeTablesFixture(t *testing.T, ddl ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "database.sqlite")
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = db.Exec(strings.Join(ddl, ";\n"))
	require.NoError(t, err)
	require.NoError(t, db.Close())
	return path
}
