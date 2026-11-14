package line

import (
	"database/sql"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/line/linetest"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestOpenResolvesFileDirectoryAndBackup(t *testing.T) {
	req := require.New(t)
	f := sampleFixture()
	dir := linetest.Write(t, t.TempDir(), f)
	backup := linetest.WriteBackup(t, t.TempDir(), f)
	remapped := linetest.WriteBackup(t, t.TempDir(), f)
	db, err := sql.Open("sqlite3", filepath.Join(remapped, "Manifest.db"))
	req.NoError(err)
	var old string
	req.NoError(db.QueryRowContext(t.Context(), `SELECT fileID FROM Files WHERE relativePath LIKE '%/Line.sqlite'`).Scan(&old))
	// A synthetic file ID checks that resolution uses the manifest entry.
	id := strings.Repeat("a", 40)
	req.NoError(os.MkdirAll(filepath.Join(remapped, id[:2]), 0755))
	req.NoError(os.Rename(filepath.Join(remapped, old[:2], old), filepath.Join(remapped, id[:2], id)))
	_, err = db.ExecContext(t.Context(), `UPDATE Files SET fileID = ? WHERE fileID = ?`, id, old)
	req.NoError(err)
	req.NoError(db.Close())
	for name, tc := range map[string]struct{ path, expected string }{
		"file":             {filepath.Join(dir, "Line.sqlite"), filepath.Join(dir, "Line.sqlite")},
		"directory":        {dir, filepath.Join(dir, "Line.sqlite")},
		"backup":           {backup, ""},
		"manifest file ID": {remapped, filepath.Join(remapped, id[:2], id)},
	} {
		t.Run(name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, err := Open(t.Context(), tc.path)
			require.NoError(err)
			t.Cleanup(func() { require.NoError(st.Close()) })
			if tc.expected != "" {
				assert.Equal(tc.expected, st.Path)
			}
			assert.NotEmpty(st.GroupsPath, "UnifiedGroup.sqlite is found beside the store")
			owner, err := st.OwnerMID(t.Context())
			require.NoError(err)
			assert.Equal(ownerMID, owner)
			users, err := st.Users(t.Context())
			require.NoError(err)
			chats, err := st.Chats(t.Context(), users)
			require.NoError(err)
			require.Len(chats, 4)
			assert.Equal("Book club", chats[2].Title)
			assert.Equal([]string{ownerMID, aliceMID, bobMID}, chats[2].Members)
			assert.Equal([]string{aliceMID, bobMID}, chats[3].Members)
		})
	}
}

func TestOpenRejectsOtherFiles(t *testing.T) {
	require := require.New(t)
	empty := t.TempDir()
	_, err := Open(t.Context(), empty)
	require.ErrorContains(err, "holds neither Line.sqlite nor Manifest.db")
	_, err = Open(t.Context(), filepath.Join(empty, "missing"))
	require.Error(err)
	other := testutil.WriteFile(t, empty, "notes.txt", []byte("not a database"))
	_, err = Open(t.Context(), other)
	require.ErrorContains(err, "not a database")
	archive := testutil.NewSQLiteTestStore(t)
	var path string
	require.NoError(archive.DB().QueryRowContext(t.Context(), "SELECT file FROM pragma_database_list WHERE name = 'main'").Scan(&path))
	_, err = Open(t.Context(), path)
	require.ErrorContains(err, "not a LINE message store")
}

func TestOpenReadsUncheckpointedWAL(t *testing.T) {
	assert, req := assert.New(t), require.New(t)
	f := sampleFixture()
	f.WALChats = []linetest.Chat{{MID: bobMID, Type: 0, Members: []string{bobMID}, Messages: []linetest.Message{
		{ID: "500", Millis: 1700000050000, Sender: bobMID, Text: "only in the write-ahead log"},
	}}}
	dir := linetest.Write(t, t.TempDir(), f)
	info, err := os.Stat(filepath.Join(dir, "Line.sqlite-wal"))
	req.NoError(err)
	req.Positive(info.Size(), "the fixture must leave rows in the log")
	for name, path := range map[string]string{"directory": dir, "backup": linetest.WriteBackup(t, t.TempDir(), f)} {
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			reader, err := Open(t.Context(), path)
			require.NoError(err)
			defer func() { require.NoError(reader.Close()) }()
			users, err := reader.Users(t.Context())
			require.NoError(err)
			chats, err := reader.Chats(t.Context(), users)
			require.NoError(err)
			var found bool
			for _, chat := range chats {
				if chat.MID == bobMID {
					require.NoError(reader.Messages(t.Context(), chat.RowID, func(m *Message) error {
						found = m.ID == "500" && m.Text == "only in the write-ahead log"
						return nil
					}))
				}
			}
			require.True(found)
		})
	}
	opened, err := Open(t.Context(), dir)
	req.NoError(err)
	req.NotEmpty(opened.scratch, "a store with a log is read from a scratch copy")
	req.DirExists(opened.scratch)
	req.NoError(opened.Close())
	assert.NoDirExists(opened.scratch, "the scratch copy is removed on Close")
	original, err := os.ReadFile(filepath.Join(dir, "Line.sqlite-wal"))
	req.NoError(err)
	assert.Len(original, int(info.Size()), "the source log is never modified")
}

func TestOpenRejectsBackupWithSeveralAccounts(t *testing.T) {
	second := linetest.Fixture{Chats: []linetest.Chat{{MID: "u0000000000000000000000000000000f", Type: 100, Messages: []linetest.Message{{ID: "1", Millis: 1, Text: "other account"}}}}}
	backup := linetest.WriteBackup(t, t.TempDir(), sampleFixture(), second)
	_, err := Open(t.Context(), backup)
	require.ErrorContains(t, err, "holds 2 LINE message stores")
	require.ErrorContains(t, err, "use a backup browser to extract the Messages directory")
}

func TestResumeTracksMessageStoreContents(t *testing.T) {
	t.Run("timestamp-only WAL", func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		f := sampleFixture()
		f.WALChats = []linetest.Chat{{MID: bobMID, Type: 0, Members: []string{bobMID}, Messages: []linetest.Message{
			{ID: "500", Millis: 1700000050000, Sender: bobMID, Text: "only in the write-ahead log"},
		}}}
		dir := linetest.Write(t, t.TempDir(), f)
		wal := filepath.Join(dir, "Line.sqlite-wal")
		st := testutil.NewTestStore(t)
		summary, err := Import(t.Context(), st, ImportOptions{Path: dir, Limit: 1})
		require.NoError(err)
		require.True(summary.LimitReached)
		info, err := os.Stat(wal)
		require.NoError(err)
		later := info.ModTime().Add(time.Hour)
		require.NoError(os.Chtimes(wal, later, later))
		summary, err = Import(t.Context(), st, ImportOptions{Path: dir})
		require.NoError(err)
		assert.True(summary.WasResumed)
		assert.EqualValues(9, summary.MessagesAdded)
	})
	for _, wal := range []bool{false, true} {
		name := "database"
		if wal {
			name = "wal"
		}
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			dir := linetest.Write(t, t.TempDir(), sampleFixture())
			path := filepath.Join(dir, "Line.sqlite")
			insert := func(path, id string, useWAL bool) *sql.DB {
				db, err := sql.Open("sqlite3", path)
				require.NoError(err)
				if useWAL {
					_, err = db.ExecContext(t.Context(), `PRAGMA journal_mode=WAL`)
					require.NoError(err)
				}
				_, err = db.ExecContext(t.Context(), `INSERT INTO ZMESSAGE (Z_PK, Z_ENT, ZCONTENTTYPE, ZSENDSTATUS, ZTIMESTAMP, ZCHAT, ZID, ZTEXT) VALUES (999, 5, 0, 1, 1700000000001, 1, ?, 'new message')`, id)
				require.NoError(err)
				return db
			}
			limit := 2
			if wal {
				db := insert(path, "901", true)
				defer func() { require.NoError(db.Close()) }()
				limit = 3
			}
			st := testutil.NewTestStore(t)
			first, err := Import(t.Context(), st, ImportOptions{Path: dir, Limit: limit})
			require.NoError(err)
			require.True(first.LimitReached)
			changed := path
			if wal {
				changed += "-wal"
			}
			before, err := os.Stat(changed)
			require.NoError(err)
			if wal {
				other := linetest.Write(t, t.TempDir(), sampleFixture())
				db := insert(filepath.Join(other, "Line.sqlite"), "902", true)
				defer func() { require.NoError(db.Close()) }()
				data, err := os.ReadFile(filepath.Join(other, "Line.sqlite-wal"))
				require.NoError(err)
				require.NoError(os.WriteFile(changed, data, 0600))
			} else {
				db := insert(path, "902", false)
				require.NoError(db.Close())
			}
			require.NoError(os.Chtimes(changed, before.ModTime(), before.ModTime()))
			after, err := os.Stat(changed)
			require.NoError(err)
			require.Equal(before.Size(), after.Size())
			require.Equal(before.ModTime(), after.ModTime())
			result, err := Import(t.Context(), st, ImportOptions{Path: dir})
			require.NoError(err)
			require.False(result.WasResumed)
			require.Equal(1, count(t, st, `SELECT count(*) FROM messages WHERE source_message_id = ?`, ownerMID+":902"))
		})
	}
}

func TestOpenUnreadableManifest(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Manifest.db"), []byte("encrypted backup"), 0600))
	_, err := Open(t.Context(), dir)
	require.ErrorContains(t, err, "encrypted backups are not supported")
}

func TestResumeTracksGroupStore(t *testing.T) {
	for _, change := range []string{"added", "same-size", "wal-appears", "same-size-wal", "mtime-only"} {
		t.Run(change, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			f := sampleFixture()
			f.Chats = []linetest.Chat{f.Chats[2], f.Chats[1]}
			f.NoGroupsFile = change == "added"
			dir := linetest.Write(t, t.TempDir(), f)
			group := filepath.Join(dir, "UnifiedGroup.sqlite")
			if change == "wal-appears" {
				db, err := sql.Open("sqlite3", group)
				require.NoError(err)
				_, err = db.ExecContext(t.Context(), `PRAGMA journal_mode=WAL`)
				require.NoError(err)
				require.NoError(db.Close())
			}
			if change == "same-size-wal" {
				db, err := sql.Open("sqlite3", group)
				require.NoError(err)
				defer func() { require.NoError(db.Close()) }()
				_, err = db.ExecContext(t.Context(), `PRAGMA journal_mode=WAL`)
				require.NoError(err)
				_, err = db.ExecContext(t.Context(), `UPDATE ZUNIFIEDGROUP SET ZNAME = 'Old  club'`)
				require.NoError(err)
			}
			st := testutil.NewTestStore(t)
			opts := ImportOptions{Path: dir, Me: ownerMID, Limit: 3}
			first, err := Import(t.Context(), st, opts)
			require.NoError(err)
			require.True(first.LimitReached)
			var cursor string
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT cursor_before FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&cursor))
			var cp lineCheckpoint
			require.NoError(json.Unmarshal([]byte(cursor), &cp))
			require.Equal(1, cp.NextChat)
			switch change {
			case "added":
				copyDir := linetest.Write(t, t.TempDir(), sampleFixture())
				data, err := os.ReadFile(filepath.Join(copyDir, "UnifiedGroup.sqlite"))
				require.NoError(err)
				require.NoError(os.WriteFile(group, data, 0600))
			case "mtime-only":
				info, err := os.Stat(group)
				require.NoError(err)
				later := info.ModTime().Add(time.Hour)
				require.NoError(os.Chtimes(group, later, later))
			case "same-size":
				before, err := os.Stat(group)
				require.NoError(err)
				db, err := sql.Open("sqlite3", group)
				require.NoError(err)
				_, err = db.ExecContext(t.Context(), `UPDATE ZUNIFIEDGROUP SET ZNAME = 'New  club'`)
				require.NoError(err)
				require.NoError(db.Close())
				require.NoError(os.Chtimes(group, before.ModTime(), before.ModTime()))
				after, err := os.Stat(group)
				require.NoError(err)
				require.Equal(before.Size(), after.Size())
				require.Equal(before.ModTime(), after.ModTime())
			case "wal-appears":
				before, err := os.ReadFile(group)
				require.NoError(err)
				info, err := os.Stat(group)
				require.NoError(err)
				db, err := sql.Open("sqlite3", group)
				require.NoError(err)
				defer func() { require.NoError(db.Close()) }()
				_, err = db.ExecContext(t.Context(), `UPDATE ZUNIFIEDGROUP SET ZNAME = 'New  club'`)
				require.NoError(err)
				after, err := os.ReadFile(group)
				require.NoError(err)
				require.Equal(before, after)
				afterInfo, err := os.Stat(group)
				require.NoError(err)
				require.Equal(info.ModTime(), afterInfo.ModTime())
				require.FileExists(group + "-wal")
			case "same-size-wal":
				wal := group + "-wal"
				info, err := os.Stat(wal)
				require.NoError(err)
				other := linetest.Write(t, t.TempDir(), f)
				db, err := sql.Open("sqlite3", filepath.Join(other, "UnifiedGroup.sqlite"))
				require.NoError(err)
				defer func() { require.NoError(db.Close()) }()
				_, err = db.ExecContext(t.Context(), `PRAGMA journal_mode=WAL`)
				require.NoError(err)
				_, err = db.ExecContext(t.Context(), `UPDATE ZUNIFIEDGROUP SET ZNAME = 'New  club'`)
				require.NoError(err)
				data, err := os.ReadFile(filepath.Join(other, "UnifiedGroup.sqlite-wal"))
				require.NoError(err)
				require.Equal(info.Size(), int64(len(data)))
				require.NoError(os.WriteFile(wal, data, 0600))
				require.NoError(os.Chtimes(wal, info.ModTime(), info.ModTime()))
				after, err := os.Stat(wal)
				require.NoError(err)
				require.Equal(info.ModTime(), after.ModTime())
			}
			opts.Limit = 0
			summary, err := Import(t.Context(), st, opts)
			require.NoError(err)
			if change == "mtime-only" {
				assert.True(summary.WasResumed)
				return
			}
			assert.False(summary.WasResumed)
			var title string
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COALESCE(title, '') FROM conversations WHERE source_conversation_id = ?`), groupMID).Scan(&title))
			if change == "added" {
				assert.Equal("Book club", title)
				assert.Equal(3, count(t, st, `SELECT count(*) FROM conversation_participants cp JOIN conversations c ON c.id = cp.conversation_id WHERE c.source_conversation_id = ?`, groupMID))
				assert.Equal(2, count(t, st, `SELECT count(*) FROM messages WHERE source_message_id IN (?, ?)`, groupMID+":200", groupMID+":201"))
			} else {
				assert.Equal("New  club", title)
			}
		})
	}
}

func TestOpenRejectsMissingListedWAL(t *testing.T) {
	require := require.New(t)
	f := sampleFixture()
	f.WALChats = []linetest.Chat{{MID: bobMID, Type: 0, Messages: []linetest.Message{{ID: "500", Text: "in WAL"}}}}
	backup := linetest.WriteBackup(t, t.TempDir(), f)
	opened, err := Open(t.Context(), backup)
	require.NoError(err)
	wal := opened.WALPath
	require.NotEmpty(wal)
	require.NoError(opened.Close())
	require.NoError(os.Remove(wal))
	_, err = Open(t.Context(), backup)
	require.ErrorContains(err, "names a write-ahead log that is missing")
}

// Core Data numbers its join tables by entity, and an app update can change
// the numbers, so members must be found whatever the numbers are.
func TestChatsFindRenumberedOrMissingMemberTables(t *testing.T) {
	for name, statements := range map[string]struct{ store, groups []string }{
		"renumbered": {
			store: []string{
				`ALTER TABLE Z_1MEMBERS RENAME TO Z_3MEMBERS`,
				`ALTER TABLE Z_3MEMBERS RENAME COLUMN Z_1CHATS TO Z_3CHATS`,
				`ALTER TABLE Z_3MEMBERS RENAME COLUMN Z_12MEMBERS TO Z_14MEMBERS`,
			},
			groups: []string{
				`ALTER TABLE Z_1MEMBERCHATUSERS RENAME TO Z_4MEMBERCHATUSERS`,
				`ALTER TABLE Z_4MEMBERCHATUSERS RENAME COLUMN Z_1GROUPS TO Z_4GROUPS`,
				`ALTER TABLE Z_4MEMBERCHATUSERS RENAME COLUMN Z_2MEMBERCHATUSERS TO Z_5MEMBERCHATUSERS`,
			},
		},
		"missing": {
			store:  []string{`DROP TABLE Z_1MEMBERS`},
			groups: []string{`DROP TABLE Z_1MEMBERCHATUSERS`},
		},
	} {
		t.Run(name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			dir := linetest.Write(t, t.TempDir(), sampleFixture())
			for file, list := range map[string][]string{"Line.sqlite": statements.store, "UnifiedGroup.sqlite": statements.groups} {
				db, err := sql.Open("sqlite3", filepath.Join(dir, file))
				require.NoError(err)
				for _, statement := range list {
					_, err := db.ExecContext(t.Context(), statement)
					require.NoError(err)
				}
				require.NoError(db.Close())
			}
			reader, err := Open(t.Context(), dir)
			require.NoError(err)
			defer func() { require.NoError(reader.Close()) }()
			users, err := reader.Users(t.Context())
			require.NoError(err)
			chats, err := reader.Chats(t.Context(), users)
			require.NoError(err)
			require.Len(chats, 4)
			assert.Equal("Book club", chats[2].Title)
			if name == "renumbered" {
				assert.Equal([]string{ownerMID, aliceMID, bobMID}, chats[2].Members)
				assert.Equal([]string{aliceMID, bobMID}, chats[3].Members)
			} else {
				assert.Empty(chats[2].Members)
				assert.Empty(chats[3].Members)
			}
		})
	}
}
