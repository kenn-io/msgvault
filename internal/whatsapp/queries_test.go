package whatsapp

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

func TestFetchChatsOldSchema(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	// Old WhatsApp schemas (pre-2022) lack the group_type column on chat.
	// fetchChats should handle this gracefully, defaulting group_type to 0.
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(err)
	defer func() { _ = db.Close() }()
	resetColumnCache()

	// Create old-style chat table without group_type.
	_, err = db.Exec(`
		CREATE TABLE jid (
			_id INTEGER PRIMARY KEY,
			user TEXT,
			server TEXT,
			raw_string TEXT
		);
		CREATE TABLE chat (
			_id INTEGER PRIMARY KEY,
			jid_row_id INTEGER UNIQUE,
			hidden INTEGER,
			subject TEXT,
			sort_timestamp INTEGER
		);

		INSERT INTO jid (_id, user, server, raw_string)
			VALUES (1, '447700900000', 's.whatsapp.net', '447700900000@s.whatsapp.net');
		INSERT INTO jid (_id, user, server, raw_string)
			VALUES (2, '120363001234567890', 'g.us', '120363001234567890@g.us');

		INSERT INTO chat (_id, jid_row_id, hidden, subject, sort_timestamp)
			VALUES (10, 1, 0, NULL, 1609459200000);
		INSERT INTO chat (_id, jid_row_id, hidden, subject, sort_timestamp)
			VALUES (20, 2, 0, 'Family Group', 1609459300000);
	`)
	require.NoError(err)

	chats, err := fetchChats(db)
	require.NoError(err, "fetchChats with old schema")

	require.Len(chats, 2)

	// All chats should have GroupType=0 since column is missing.
	for _, c := range chats {
		assert.Equal(0, c.GroupType, "chat %d: GroupType", c.RowID)
	}

	// Group chat (g.us) should still be detected via server.
	group := chats[0] // sorted by sort_timestamp DESC
	assert.Equal("g.us", group.Server, "expected first chat to be group (g.us)")
	assert.True(isGroupChat(group), "g.us chat should be detected as group even without group_type column")
}

func TestFetchChatsNewSchema(t *testing.T) {
	require := requirepkg.New(t)
	// New WhatsApp schemas have group_type on chat.
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(err)
	defer func() { _ = db.Close() }()
	resetColumnCache()

	_, err = db.Exec(`
		CREATE TABLE jid (
			_id INTEGER PRIMARY KEY,
			user TEXT,
			server TEXT,
			raw_string TEXT
		);
		CREATE TABLE chat (
			_id INTEGER PRIMARY KEY,
			jid_row_id INTEGER UNIQUE,
			hidden INTEGER,
			subject TEXT,
			sort_timestamp INTEGER,
			group_type INTEGER
		);

		INSERT INTO jid (_id, user, server, raw_string)
			VALUES (1, '120363009999', 'g.us', '120363009999@g.us');
		INSERT INTO chat (_id, jid_row_id, hidden, subject, sort_timestamp, group_type)
			VALUES (10, 1, 0, 'Work Chat', 1609459200000, 1);
	`)
	require.NoError(err)

	chats, err := fetchChats(db)
	require.NoError(err, "fetchChats with new schema")

	require.Len(chats, 1)
	assertpkg.Equal(t, 1, chats[0].GroupType, "GroupType")
}

func TestFetchMediaOldSchema(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	// Old WhatsApp schemas lack media_caption on message_media.
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(err)
	defer func() { _ = db.Close() }()
	resetColumnCache()

	_, err = db.Exec(`
		CREATE TABLE message_media (
			message_row_id INTEGER PRIMARY KEY,
			mime_type TEXT,
			file_size INTEGER,
			file_path TEXT,
			width INTEGER,
			height INTEGER,
			media_duration INTEGER
		);

		INSERT INTO message_media (message_row_id, mime_type, file_size, file_path, width, height, media_duration)
			VALUES (100, 'image/jpeg', 54321, 'Media/IMG-20200101.jpg', 1920, 1080, 0);
	`)
	require.NoError(err)

	mediaMap, err := fetchMedia(db, []int64{100})
	require.NoError(err, "fetchMedia with old schema")

	m, ok := mediaMap[100]
	require.True(ok, "expected media for message 100")
	assert.False(m.MediaCaption.Valid, "MediaCaption should be NULL for old schema")
	assert.True(m.MimeType.Valid, "MimeType valid")
	assert.Equal("image/jpeg", m.MimeType.String, "MimeType")
}

func TestColumnCacheScopedPerDB(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	// Verify that inspecting an old-schema DB then a new-schema DB
	// (and vice versa) produces correct results without resetColumnCache.
	resetColumnCache()

	// DB 1: old schema, no group_type.
	oldDB, err := sql.Open("sqlite3", ":memory:")
	require.NoError(err)
	defer func() { _ = oldDB.Close() }()
	_, err = oldDB.Exec(`
		CREATE TABLE jid (_id INTEGER PRIMARY KEY, user TEXT, server TEXT, raw_string TEXT);
		CREATE TABLE chat (_id INTEGER PRIMARY KEY, jid_row_id INTEGER UNIQUE, hidden INTEGER, subject TEXT, sort_timestamp INTEGER);
		INSERT INTO jid VALUES (1, '441234567890', 's.whatsapp.net', '441234567890@s.whatsapp.net');
		INSERT INTO chat VALUES (1, 1, 0, NULL, 1000);
	`)
	require.NoError(err)

	// DB 2: new schema, has group_type.
	newDB, err := sql.Open("sqlite3", ":memory:")
	require.NoError(err)
	defer func() { _ = newDB.Close() }()
	_, err = newDB.Exec(`
		CREATE TABLE jid (_id INTEGER PRIMARY KEY, user TEXT, server TEXT, raw_string TEXT);
		CREATE TABLE chat (_id INTEGER PRIMARY KEY, jid_row_id INTEGER UNIQUE, hidden INTEGER, subject TEXT, sort_timestamp INTEGER, group_type INTEGER);
		INSERT INTO jid VALUES (1, '120363009999', 'g.us', '120363009999@g.us');
		INSERT INTO chat VALUES (1, 1, 0, 'Test Group', 2000, 3);
	`)
	require.NoError(err)

	// Query old DB first — should NOT cache "no group_type" for new DB.
	oldChats, err := fetchChats(oldDB)
	require.NoError(err, "old DB")
	assert.Equal(0, oldChats[0].GroupType, "old DB: GroupType")

	// Query new DB — must see group_type despite old DB being queried first.
	newChats, err := fetchChats(newDB)
	require.NoError(err, "new DB")
	assert.Equal(3, newChats[0].GroupType, "new DB: GroupType")

	// Reverse: query new DB again then old DB again — still correct.
	newChats2, err := fetchChats(newDB)
	require.NoError(err, "new DB (2nd)")
	assert.Equal(3, newChats2[0].GroupType, "new DB (2nd): GroupType")

	oldChats2, err := fetchChats(oldDB)
	require.NoError(err, "old DB (2nd)")
	assert.Equal(0, oldChats2[0].GroupType, "old DB (2nd): GroupType")
}

func TestFetchLidMap(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(err)
	defer func() { _ = db.Close() }()

	// Create the jid and jid_map tables matching WhatsApp's actual schema.
	// In WhatsApp: jid_map.lid_row_id is PK (= jid._id for the lid entry),
	// jid_map.jid_row_id points to the phone jid._id.
	_, err = db.Exec(`
		CREATE TABLE jid (
			_id INTEGER PRIMARY KEY,
			user TEXT,
			server TEXT,
			raw_string TEXT
		);
		CREATE TABLE jid_map (
			lid_row_id INTEGER PRIMARY KEY NOT NULL,
			jid_row_id INTEGER NOT NULL
		);

		-- lid JID entries (these are the lid_row_id values)
		INSERT INTO jid (_id, user, server, raw_string) VALUES (10, '12345abcde', 'lid', '12345abcde@lid');
		INSERT INTO jid (_id, user, server, raw_string) VALUES (20, '67890fghij', 'lid', '67890fghij@lid');

		-- phone JID entries (these are the jid_row_id values)
		INSERT INTO jid (_id, user, server, raw_string) VALUES (11, '447957366403', 's.whatsapp.net', '447957366403@s.whatsapp.net');
		INSERT INTO jid (_id, user, server, raw_string) VALUES (21, '12025551234', 's.whatsapp.net', '12025551234@s.whatsapp.net');

		-- Map lid → phone
		INSERT INTO jid_map (lid_row_id, jid_row_id) VALUES (10, 11);
		INSERT INTO jid_map (lid_row_id, jid_row_id) VALUES (20, 21);
	`)
	require.NoError(err)

	lidMap, err := fetchLidMap(db)
	require.NoError(err)

	require.Len(lidMap, 2)

	m1, ok := lidMap[10]
	require.True(ok, "expected lid row 10 in map")
	assert.Equal("447957366403", m1.PhoneUser, "lid 10")
	assert.Equal("s.whatsapp.net", m1.PhoneServer, "lid 10")

	m2, ok := lidMap[20]
	require.True(ok, "expected lid row 20 in map")
	assert.Equal("12025551234", m2.PhoneUser, "lid 20")
}

func TestFetchLidMapMissingTable(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	requirepkg.NoError(t, err)
	defer func() { _ = db.Close() }()

	// No jid_map table — should return empty map, not error.
	lidMap, err := fetchLidMap(db)
	requirepkg.NoError(t, err, "expected no error for missing table")
	assertpkg.Empty(t, lidMap, "expected empty map")
}
