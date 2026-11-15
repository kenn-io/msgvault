// Package linetest builds synthetic LINE iOS stores for tests. The tables
// carry the columns of the app's Core Data schema that the importer reads.
package linetest

import (
	"crypto/sha1" //nolint:gosec // iOS backups name files by the SHA-1 of domain and path
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	_ "github.com/mattn/go-sqlite3" // registers the sqlite3 driver the fixtures are written with
	"github.com/stretchr/testify/require"
	"howett.net/plist"
)

// User is one ZUSER row.
type User struct {
	MID             string
	Name            string
	CustomName      string
	AddressBookName string
}

// Message is one ZMESSAGE row. Sender "" means the owner sent it. NoID leaves
// ZID NULL, as LINE does for a send that never completed.
type Message struct {
	ID          string
	NoID        bool
	ContentType int
	Millis      int64
	Sender      string
	Text        string
	Metadata    map[string]string
}

// Chat is one ZCHAT row. Type follows ZCHAT.ZTYPE: 0 direct, 1 room, 2 group,
// 100 the owner's Keep Memo chat. Members fill Z_1MEMBERS for rooms and the
// UnifiedGroup store for groups.
type Chat struct {
	MID      string
	Type     int
	Name     string
	Members  []string
	Messages []Message
}

// Fixture describes a store. Groups are written to UnifiedGroup.sqlite unless
// NoGroupsFile is set. WALChats are written only to the store's write-ahead
// log, left uncheckpointed beside it as Line.sqlite-wal, the way a backup of
// a running app can carry them.
type Fixture struct {
	Users        []User
	Chats        []Chat
	WALChats     []Chat
	NoGroupsFile bool
}

const storeSchema = `
CREATE TABLE ZCHAT ( Z_PK INTEGER PRIMARY KEY, Z_ENT INTEGER, Z_OPT INTEGER, ZALERT INTEGER, ZE2EECONTENTTYPES INTEGER, ZENABLE INTEGER, ZLASTRECEIVEDMESSAGEID INTEGER, ZLIVE INTEGER, ZREADUPTOMESSAGEID INTEGER, ZREADUPTOMESSAGEIDSYNCED INTEGER, ZSESSIONID INTEGER, ZSORTORDER INTEGER, ZTYPE INTEGER, ZUNREAD INTEGER, ZMETADATA INTEGER, ZEXPIREINTERVAL FLOAT, ZLASTUPDATED TIMESTAMP, ZINPUTTEXT VARCHAR, ZINVITERMID VARCHAR, ZLASTMESSAGE VARCHAR, ZMID VARCHAR, ZSKIN VARCHAR );
CREATE TABLE Z_1MEMBERS ( Z_1CHATS INTEGER, Z_12MEMBERS INTEGER, PRIMARY KEY (Z_1CHATS, Z_12MEMBERS) );
CREATE TABLE ZMESSAGE ( Z_PK INTEGER PRIMARY KEY, Z_ENT INTEGER, Z_OPT INTEGER, ZCONTENTTYPE INTEGER, ZREADCOUNT INTEGER, ZSENDSTATUS INTEGER, ZTIMESTAMP INTEGER, ZCHAT INTEGER, ZSENDER INTEGER, ZLATITUDE FLOAT, ZLONGITUDE FLOAT, ZID VARCHAR, ZMESSAGETYPE VARCHAR, ZTEXT VARCHAR, ZCONTENTMETADATA BLOB, ZTHUMBNAIL BLOB );
CREATE TABLE ZUSER ( Z_PK INTEGER PRIMARY KEY, Z_ENT INTEGER, Z_OPT INTEGER, ZALERT INTEGER, ZBLOCKING INTEGER, ZCAPABILITIES INTEGER, ZCONTACTTYPE INTEGER, ZE2EECONTENTTYPES INTEGER, ZFAVORITEORDER INTEGER, ZFRIENDREQUESTSTATUS INTEGER, ZISFRIEND INTEGER, ZISHIDDEN INTEGER, ZISINADDRESSBOOK INTEGER, ZISRECOMMENDED INTEGER, ZISREMOVED INTEGER, ZISUNREGISTERED INTEGER, ZISVIEWED INTEGER, ZMYHOMECAPABLE INTEGER, ZUSERTYPE INTEGER, ZVIDEOCALLCAPABLE INTEGER, ZVOICECALLCAPABLE INTEGER, ZCREATEDTIME TIMESTAMP, ZSTATUSUPDATEDAT TIMESTAMP, ZADDRESSBOOKNAME VARCHAR, ZCOUNTRY VARCHAR, ZCUSTOMNAME VARCHAR, ZMID VARCHAR, ZNAME VARCHAR, ZPICTURESTATUS VARCHAR, ZPICTUREURL VARCHAR, ZPROFILEIMAGE VARCHAR, ZRECOMMENDPARAMS VARCHAR, ZSORTABLENAME VARCHAR, ZSTATUSMESSAGE VARCHAR, ZE2EEPUBLICKEYCHAIN BLOB, ZPUBLICKEYCHAIN BLOB );
CREATE INDEX Z_Message_chat_timestamp ON ZMESSAGE (ZCHAT COLLATE BINARY ASC, ZTIMESTAMP COLLATE BINARY ASC);
`

const groupsSchema = `
CREATE TABLE ZUNIFIEDGROUP ( Z_PK INTEGER PRIMARY KEY, Z_ENT INTEGER, Z_OPT INTEGER, ZALERT INTEGER, ZENABLE INTEGER, ZFAVORITEORDER INTEGER, ZISACCEPTED INTEGER, ZISADDFRIENDDISABLED INTEGER, ZISPREVENTLINKINVITATION INTEGER, ZISTICKETDISABLED INTEGER, ZISVIEWED INTEGER, ZTYPE INTEGER, ZCREATORCHATUSER INTEGER, ZCREATEDTIME TIMESTAMP, ZID VARCHAR, ZLINKINVITATION VARCHAR, ZNAME VARCHAR, ZPICTURESTATUS VARCHAR , ZMEMBERSCOUNT INTEGER, ZISAUTONAME INTEGER, ZISPENALIZED INTEGER);
CREATE TABLE Z_1MEMBERCHATUSERS ( Z_1GROUPS INTEGER, Z_2MEMBERCHATUSERS INTEGER, PRIMARY KEY (Z_1GROUPS, Z_2MEMBERCHATUSERS) );
CREATE TABLE ZUNIFIEDGROUPUSER ( Z_PK INTEGER PRIMARY KEY, Z_ENT INTEGER, Z_OPT INTEGER, ZMID VARCHAR );
`

// Write creates Line.sqlite (and UnifiedGroup.sqlite) in dir and returns dir.
func Write(tb testing.TB, dir string, f Fixture) string {
	tb.Helper()
	writeStore(tb, filepath.Join(dir, "Line.sqlite"), f)
	if !f.NoGroupsFile {
		writeGroups(tb, filepath.Join(dir, "UnifiedGroup.sqlite"), f)
	}
	return dir
}

// WriteBackup lays the fixtures out as an unencrypted iPhone backup under
// root: a Manifest.db plus each account's stores under their hashed names.
// Every fixture after the first is a further account directory, as a phone
// that signed in to more than one LINE account leaves behind.
func WriteBackup(tb testing.TB, root string, accounts ...Fixture) string {
	tb.Helper()
	ctx := tb.Context()
	const domain = "AppDomainGroup-group.com.linecorp.line"
	db, err := sql.Open("sqlite3", filepath.Join(root, "Manifest.db"))
	require.NoError(tb, err)
	_, err = db.ExecContext(ctx, `CREATE TABLE Files (fileID TEXT PRIMARY KEY, domain TEXT, relativePath TEXT, flags INTEGER, file BLOB)`)
	require.NoError(tb, err)
	for i, f := range accounts {
		dir := fmt.Sprintf("Library/Application Support/PrivateStore/P_u%032d/Messages/", i)
		staging := tb.TempDir()
		Write(tb, staging, f)
		entries, err := os.ReadDir(staging)
		require.NoError(tb, err)
		for _, entry := range entries {
			rel := dir + entry.Name()
			sum := sha1.Sum([]byte(domain + "-" + rel)) //nolint:gosec // the backup format's file naming
			id := hex.EncodeToString(sum[:])
			require.NoError(tb, os.MkdirAll(filepath.Join(root, id[:2]), 0o755))
			copyFixtureFile(tb, filepath.Join(staging, entry.Name()), filepath.Join(root, id[:2], id))
			_, err := db.ExecContext(ctx, `INSERT INTO Files (fileID, domain, relativePath, flags) VALUES (?, ?, ?, 1)`, id, domain, rel)
			require.NoError(tb, err)
		}
	}
	require.NoError(tb, db.Close())
	return root
}

func writeStore(tb testing.TB, path string, f Fixture) {
	tb.Helper()
	ctx := tb.Context()
	db, err := sql.Open("sqlite3", path)
	require.NoError(tb, err)
	_, err = db.ExecContext(ctx, storeSchema)
	require.NoError(tb, err)
	userRow := map[string]int64{}
	for i, u := range f.Users {
		row := int64(i + 1)
		userRow[u.MID] = row
		_, err := db.ExecContext(ctx, `INSERT INTO ZUSER (Z_PK, Z_ENT, ZMID, ZNAME, ZCUSTOMNAME, ZADDRESSBOOKNAME) VALUES (?, 12, ?, ?, ?, ?)`,
			row, u.MID, nullable(u.Name), nullable(u.CustomName), nullable(u.AddressBookName))
		require.NoError(tb, err)
	}
	next := insertChats(tb, db, userRow, f.Chats, counters{})
	require.NoError(tb, db.Close())
	if len(f.WALChats) == 0 {
		return
	}
	// Rows written in WAL mode stay in the log until a checkpoint; copying
	// the database and its log while the writer is still open leaves them
	// there, as a backup of a running app does.
	staging := filepath.Join(tb.TempDir(), "Line.sqlite")
	copyFixtureFile(tb, path, staging)
	require.NoError(tb, os.Remove(path))
	wal, err := sql.Open("sqlite3", "file:"+staging+"?_journal_mode=WAL")
	require.NoError(tb, err)
	wal.SetMaxOpenConns(1)
	insertChats(tb, wal, userRow, f.WALChats, next)
	copyFixtureFile(tb, staging, path)
	copyFixtureFile(tb, staging+"-wal", path+"-wal")
	require.NoError(tb, wal.Close())
}

// counters carries the next free ZCHAT and ZMESSAGE row ids between passes.
type counters struct{ chat, message int64 }

func insertChats(tb testing.TB, db *sql.DB, userRow map[string]int64, chats []Chat, start counters) counters {
	tb.Helper()
	ctx := tb.Context()
	next := start
	for _, c := range chats {
		next.chat++
		_, err := db.ExecContext(ctx, `INSERT INTO ZCHAT (Z_PK, Z_ENT, ZTYPE, ZMID, ZENABLE) VALUES (?, 1, ?, ?, 1)`, next.chat, c.Type, c.MID)
		require.NoError(tb, err)
		if c.Type != 2 {
			for _, mid := range c.Members {
				row, ok := userRow[mid]
				require.True(tb, ok, "member %s is not a fixture user", mid)
				_, err := db.ExecContext(ctx, `INSERT INTO Z_1MEMBERS (Z_1CHATS, Z_12MEMBERS) VALUES (?, ?)`, next.chat, row)
				require.NoError(tb, err)
			}
		}
		for _, m := range c.Messages {
			next.message++
			var sender any
			if m.Sender != "" {
				row, ok := userRow[m.Sender]
				require.True(tb, ok, "sender %s is not a fixture user", m.Sender)
				sender = row
			}
			var id any
			status := 1
			if !m.NoID {
				id = m.ID
			} else {
				status = 0
			}
			var blob any
			if len(m.Metadata) > 0 {
				blob = KeyedArchive(tb, m.Metadata)
			}
			_, err := db.ExecContext(ctx, `INSERT INTO ZMESSAGE (Z_PK, Z_ENT, ZCONTENTTYPE, ZSENDSTATUS, ZTIMESTAMP, ZCHAT, ZSENDER, ZID, ZTEXT, ZCONTENTMETADATA)
				VALUES (?, 5, ?, ?, ?, ?, ?, ?, ?, ?)`, next.message, m.ContentType, status, m.Millis, next.chat, sender, id, nullable(m.Text), blob)
			require.NoError(tb, err)
		}
	}
	return next
}

func copyFixtureFile(tb testing.TB, from, to string) {
	tb.Helper()
	data, err := os.ReadFile(from)
	require.NoError(tb, err)
	require.NoError(tb, os.WriteFile(to, data, 0o600)) //nolint:gosec // fixture paths come from the test
}

func writeGroups(tb testing.TB, path string, f Fixture) {
	tb.Helper()
	ctx := tb.Context()
	db, err := sql.Open("sqlite3", path)
	require.NoError(tb, err)
	defer func() { require.NoError(tb, db.Close()) }()
	_, err = db.ExecContext(ctx, groupsSchema)
	require.NoError(tb, err)
	userRow := map[string]int64{}
	groupRow := int64(0)
	for _, c := range append(append([]Chat{}, f.Chats...), f.WALChats...) {
		if c.Type != 2 {
			continue
		}
		groupRow++
		_, err := db.ExecContext(ctx, `INSERT INTO ZUNIFIEDGROUP (Z_PK, Z_ENT, ZID, ZNAME) VALUES (?, 1, ?, ?)`, groupRow, c.MID, nullable(c.Name))
		require.NoError(tb, err)
		for _, mid := range c.Members {
			row, ok := userRow[mid]
			if !ok {
				row = int64(len(userRow) + 1)
				userRow[mid] = row
				_, err := db.ExecContext(ctx, `INSERT INTO ZUNIFIEDGROUPUSER (Z_PK, Z_ENT, ZMID) VALUES (?, 2, ?)`, row, mid)
				require.NoError(tb, err)
			}
			_, err := db.ExecContext(ctx, `INSERT INTO Z_1MEMBERCHATUSERS (Z_1GROUPS, Z_2MEMBERCHATUSERS) VALUES (?, ?)`, groupRow, row)
			require.NoError(tb, err)
		}
	}
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// KeyedArchive encodes a string dictionary the way NSKeyedArchiver does:
// a binary plist whose $objects table holds the keys and values, with the
// root dictionary referring to them by UID.
func KeyedArchive(tb testing.TB, entries map[string]string) []byte {
	tb.Helper()
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	objects := []any{"$null"}
	var keyUIDs, valueUIDs []plist.UID
	for _, k := range keys {
		objects = append(objects, k)
		keyUIDs = append(keyUIDs, plist.UID(len(objects)-1)) //nolint:gosec // small test slice
		objects = append(objects, entries[k])
		valueUIDs = append(valueUIDs, plist.UID(len(objects)-1)) //nolint:gosec // small test slice
	}
	classUID := plist.UID(len(objects) + 1)
	objects = append(objects, map[string]any{"NS.keys": keyUIDs, "NS.objects": valueUIDs, "$class": classUID})
	rootUID := plist.UID(len(objects) - 1) //nolint:gosec // small test slice
	objects = append(objects, map[string]any{"$classname": "NSMutableDictionary", "$classes": []string{"NSMutableDictionary", "NSDictionary", "NSObject"}})
	archive := map[string]any{
		"$version":  100000,
		"$archiver": "NSKeyedArchiver",
		"$top":      map[string]any{"root": rootUID},
		"$objects":  objects,
	}
	blob, err := plist.Marshal(archive, plist.BinaryFormat)
	require.NoError(tb, err)
	return blob
}
