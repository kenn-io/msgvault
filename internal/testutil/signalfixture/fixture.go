package signalfixture

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Register the SQLite driver for fixture databases.
	"github.com/stretchr/testify/require"
)

// Synthetic identities. The phone numbers use the reserved 555-01xx range.
const (
	OwnPhone   = "+15555550100"
	AlicePhone = "+15555550101"
	CarolPhone = "+15555550103"
	DanaPhone  = "+15555550104"
	DanaPNI    = "0f0f0f0f-0000-4000-8000-000000000005"
	CarolACI   = "0f0f0f0f-0000-4000-8000-000000000004"
	OwnACI     = "0f0f0f0f-0000-4000-8000-000000000001"
	AliceACI   = "0f0f0f0f-0000-4000-8000-000000000002"
	BobACI     = "0f0f0f0f-0000-4000-8000-000000000003"
	GroupHex   = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	// GroupB64 is GroupHex as unpadded base64url.
	GroupB64 = "ABEiM0RVZneImaq7zN3u_wARIjNEVWZ3iJmqu8zd7v8"

	TypeIncoming     = 20 | 0x200000 | 0x800000
	TypeOutgoing     = 23 | 0x200000 | 0x800000
	TypeSMSIncoming  = 20
	TypeCallIncoming = 1
	TypeGroupUpdate  = 20 | 0x10000 | 0x80000 | 0x200000 | 0x800000

	TAlice1  = int64(1_700_000_000_000)
	TReply   = int64(1_700_000_060_000)
	TEditOld = int64(1_700_000_120_000)
	TEditNew = int64(1_700_000_180_000)
	// TQuoteDraft lies between the draft and its edit.
	TQuoteDraft = int64(1_700_000_150_000)
	TGroup      = int64(1_700_000_240_000)
	TCarol      = int64(1_700_000_300_000)
	TSMS        = int64(1_700_000_360_000)
)

// Layout selects the column names of a Signal database version.
type Layout struct {
	aci, e164, attachment, attachmentMessage, attachmentType string
	version                                                  int
}

var (
	// Current matches the reader columns at database version 330.
	Current = Layout{"aci", "e164", "attachment", "message_id", "content_type", 330}
	// V215 covers the attachment-table rename.
	V215 = Layout{"aci", "e164", "attachment", "message_id", "content_type", 215}
	// V185 matches database versions 185 to 199.
	V185 = Layout{"uuid", "phone", "part", "mid", "ct", 185}
)

// Write writes a small Signal Android database and an
// attachment file into dir. offset shifts every local row ID, as restoring a
// backup on another phone can.
func Write(tb testing.TB, dir string, layout Layout, offset int64) string {
	tb.Helper()
	path := filepath.Join(dir, "database.sqlite")
	db, err := sql.Open("sqlite3", path)
	require.NoError(tb, err)
	defer func() { require.NoError(tb, db.Close()) }()

	id := func(n int64) int64 { return n + offset }
	version := layout.version
	require.NoError(tb, os.WriteFile(filepath.Join(dir, "DatabaseVersion.sbf"), fmt.Appendf(nil, "VERSION:uint32:%d\n", version), 0o600))
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE recipient (_id INTEGER PRIMARY KEY, type INTEGER DEFAULT 0,
			%s TEXT, %s TEXT, pni TEXT, group_id TEXT, distribution_list_id INTEGER,
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
		require.NoError(tb, err, stmt)
	}

	if layout == Current {
		// V223 adds nickname precedence; V302 replaces remote_deleted semantics.
		for _, stmt := range []string{
			`ALTER TABLE recipient ADD COLUMN nickname_joined_name TEXT DEFAULT NULL`,
			`ALTER TABLE recipient ADD COLUMN profile_given_name TEXT DEFAULT NULL`,
			`ALTER TABLE message ADD COLUMN deleted_by INTEGER DEFAULT NULL REFERENCES recipient (_id) ON DELETE CASCADE`,
		} {
			_, err := db.Exec(stmt)
			require.NoError(tb, err, stmt)
		}
	}

	exec := func(query string, args ...any) {
		tb.Helper()
		_, err := db.Exec(query, args...)
		require.NoError(tb, err, query)
	}
	recipientSQL := fmt.Sprintf(`INSERT INTO recipient (_id, %s, %s, group_id, system_joined_name, profile_joined_name)
		VALUES (?, ?, ?, ?, ?, ?)`, layout.e164, layout.aci)
	exec(recipientSQL, id(1), OwnPhone, OwnACI, nil, nil, "Owner")
	exec(recipientSQL, id(2), AlicePhone, AliceACI, nil, "Alice Example", nil)
	exec(recipientSQL, id(3), nil, BobACI, nil, nil, "Bob Example")
	exec(recipientSQL, id(4), CarolPhone, CarolACI, nil, "Carol Example", nil)
	exec(recipientSQL, id(5), nil, nil, "__signal_group__v2__!"+GroupHex, nil, nil)
	exec(recipientSQL, id(6), nil, nil, "__textsecure_group__!00ff", nil, nil)

	exec(`INSERT INTO groups (group_id, recipient_id, title) VALUES (?, ?, ?), (?, ?, ?)`,
		"__signal_group__v2__!"+GroupHex, id(5), "Example Group", "__textsecure_group__!00ff", id(6), "Old Group")
	exec(`INSERT INTO group_membership (group_id, recipient_id) VALUES (?, ?), (?, ?), (?, ?)`,
		"__signal_group__v2__!"+GroupHex, id(1), "__signal_group__v2__!"+GroupHex, id(2), "__signal_group__v2__!"+GroupHex, id(3))

	exec(`INSERT INTO thread (_id, recipient_id) VALUES (?, ?), (?, ?), (?, ?), (?, ?)`,
		id(10), id(2), id(11), id(5), id(12), id(4), id(13), id(6))

	msgSQL := `INSERT INTO message (_id, date_sent, date_received, thread_id, from_recipient_id,
		to_recipient_id, type, body, quote_id, quote_author, latest_revision_id, original_message_id,
		revision_number) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	// Direct chat with Alice: a message, a reply quoting it, and an edit.
	exec(msgSQL, id(100), TAlice1, TAlice1+1, id(10), id(2), id(1), TypeIncoming, "Hello from Alice", 0, 0, nil, nil, 0)
	exec(msgSQL, id(101), TReply, TReply+1, id(10), id(1), id(2), TypeOutgoing, "Replying", TAlice1, id(2), nil, nil, 0)
	exec(msgSQL, id(102), TEditOld, TEditOld+1, id(10), id(1), id(2), TypeOutgoing, "Frist draft", 0, 0, id(106), nil, 0)
	// Alice quotes the draft before it is edited, so her reply's row
	// precedes the draft's newest revision.
	exec(msgSQL, id(103), TQuoteDraft, TQuoteDraft+1, id(10), id(2), id(1), TypeIncoming, "Nice draft", TEditOld, id(1), nil, nil, 0)
	exec(msgSQL, id(106), TEditNew, TEditNew+1, id(10), id(1), id(2), TypeOutgoing, "First draft", 0, 0, nil, id(102), 1)
	exec(msgSQL, id(104), TSMS, TSMS+1, id(10), id(2), id(1), TypeSMSIncoming, "Plain SMS", 0, 0, nil, nil, 0)
	exec(msgSQL, id(105), TSMS+1, TSMS+2, id(10), id(2), id(1), TypeCallIncoming, nil, 0, 0, nil, nil, 0)
	// Group chat: a message from Bob mentioning Alice, with a photo.
	exec(msgSQL, id(200), TGroup, TGroup+1, id(11), id(3), id(5), TypeIncoming, "Hi \uFFFC, look", 0, 0, nil, nil, 0)
	exec(msgSQL, id(201), TGroup+5, TGroup+6, id(11), id(1), id(5), TypeGroupUpdate, nil, 0, 0, nil, nil, 0)
	// Another direct chat.
	exec(msgSQL, id(300), TCarol, TCarol+1, id(12), id(4), id(1), TypeIncoming, "Carol here", 0, 0, nil, nil, 0)
	// Legacy group.
	exec(msgSQL, id(400), TCarol+10, TCarol+11, id(13), id(4), id(6), TypeIncoming, "Old group text", 0, 0, nil, nil, 0)

	exec(`INSERT INTO mention (thread_id, message_id, recipient_id, range_start, range_length) VALUES (?, ?, ?, 3, 1)`,
		id(11), id(200), id(2))
	exec(`INSERT INTO reaction (message_id, author_id, emoji, date_sent, date_received) VALUES (?, ?, ?, ?, ?), (?, ?, ?, ?, ?)`,
		id(200), id(2), "\U0001F44D", TGroup+100, TGroup+101,
		id(106), id(2), "\u2764", TEditNew+100, TEditNew+101)

	attachmentSQL := fmt.Sprintf(`INSERT INTO %s (_id, %s, %s, file_name, data_size, width, height, caption, quote)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, layout.attachment, layout.attachmentMessage, layout.attachmentType)
	exec(attachmentSQL, id(500), id(200), "image/jpeg", nil, 11, 640, 480, "Beach", 0)
	exec(attachmentSQL, id(501), id(101), "image/jpeg", nil, 5, 0, 0, nil, 1)
	exec(attachmentSQL, id(502), id(300), "application/pdf", "notes.pdf", 9, 0, 0, nil, 0)

	// Only the photo's file is present; the PDF was never downloaded.
	require.NoError(tb, os.WriteFile(filepath.Join(dir, fmt.Sprintf("Attachment_%d_-1.bin", id(500))),
		[]byte("photo bytes"), 0o600))
	require.NoError(tb, os.WriteFile(filepath.Join(dir, fmt.Sprintf("Attachment_%d_-1.sbf", id(500))),
		[]byte("frame"), 0o600))
	return path
}

// WritePNIOnly writes a backup with one outgoing message to a contact whose
// account ID Signal has yet to learn.
func WritePNIOnly(tb testing.TB, dir string, layout Layout) string {
	tb.Helper()
	path := Write(tb, dir, layout, 0)
	db, err := sql.Open("sqlite3", path)
	require.NoError(tb, err)
	defer func() { require.NoError(tb, db.Close()) }()
	pni := DanaPNI
	if layout.version >= 200 {
		pni = "PNI:" + pni
	}
	for _, stmt := range []string{`DELETE FROM message`, `DELETE FROM thread`} {
		_, err := db.Exec(stmt)
		require.NoError(tb, err)
	}
	_, err = db.Exec(fmt.Sprintf(`INSERT INTO recipient (_id, %s, pni, system_joined_name) VALUES (7, ?, ?, 'Dana Example')`, layout.e164), DanaPhone, pni)
	require.NoError(tb, err)
	_, err = db.Exec(`INSERT INTO thread (_id, recipient_id) VALUES (14, 7)`)
	require.NoError(tb, err)
	_, err = db.Exec(`INSERT INTO message (_id, date_sent, date_received, thread_id, from_recipient_id, to_recipient_id, type, body)
		VALUES (600, 1700000400000, 1700000400001, 14, 1, 7, ?, 'hello Dana')`, TypeOutgoing)
	require.NoError(tb, err)
	return path
}
