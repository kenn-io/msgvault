package macoscalls

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/callhistory"
)

func sourceDB(t *testing.T, schema string) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "Call History #.storedata")
	db, err := sql.Open("sqlite3", fileDSN(path, "rwc"))
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	_, err = db.Exec(schema)
	require.NoError(t, err)
	return db, path
}

func TestReadWALSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	db, path := sourceDB(t, `PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0;
 CREATE TABLE ZCALLRECORD(Z_PK INTEGER PRIMARY KEY,ZUNIQUE_ID TEXT,ZDATE TIMESTAMP,ZDURATION REAL,ZORIGINATED INTEGER,ZANSWERED INTEGER,ZADDRESS TEXT,ZNAME TEXT,ZHANDLE_TYPE INTEGER,ZSERVICE_PROVIDER TEXT,ZCALLTYPE INTEGER,ZPARTICIPANTGROUPUUID TEXT,ZORIGINATINGDEVICENAME TEXT,ZDISCONNECTED_CAUSE INTEGER);
 INSERT INTO ZCALLRECORD VALUES(1,'committed',0.125,5.25,0,1,'peer@example.com','Example',1,'com.apple.FaceTime',8,NULL,'Example device',0);`)
	tx, err := db.Begin()
	require.NoError(err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`INSERT INTO ZCALLRECORD(Z_PK,ZUNIQUE_ID,ZDATE,ZANSWERED) VALUES(2,'pending',0,1)`)
	require.NoError(err)
	before, err := os.ReadFile(path)
	require.NoError(err)
	wal, err := os.ReadFile(path + "-wal")
	require.NoError(err)
	var got []callhistory.Observation
	require.NoError(Read(t.Context(), path, "example", func(o callhistory.Observation) error { got = append(got, o); return nil }))
	require.Len(got, 1)
	assert.Equal("committed", got[0].RecordID)
	assert.Equal(125000000, got[0].OccurredAt.Nanosecond())
	assert.JSONEq(`{"ZUNIQUE_ID":"committed","ZDATE":0.125,"ZDURATION":5.25,"ZORIGINATED":0,"ZANSWERED":1,"ZADDRESS":"peer@example.com","ZNAME":"Example","ZHANDLE_TYPE":1,"ZSERVICE_PROVIDER":"com.apple.FaceTime","ZCALLTYPE":8,"ZPARTICIPANTGROUPUUID":null,"ZORIGINATINGDEVICENAME":"Example device","ZDISCONNECTED_CAUSE":0}`, string(got[0].Raw))
	after, err := os.ReadFile(path)
	require.NoError(err)
	assert.Equal(before, after)
	after, err = os.ReadFile(path + "-wal")
	require.NoError(err)
	assert.Equal(wal, after)
	entries, err := os.ReadDir(temp)
	require.NoError(err)
	assert.Empty(entries)
}

func TestReadFailuresAndCleanup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, schema := range []string{
		`CREATE TABLE unrelated(x);`,
		`CREATE TABLE ZCALLRECORD(Z_PK INTEGER PRIMARY KEY,ZDATE REAL);`,
		`CREATE TABLE ZCALLRECORD(Z_PK INTEGER PRIMARY KEY,ZUNIQUE_ID TEXT,ZDATE REAL); INSERT INTO ZCALLRECORD VALUES(1,'secret',NULL);`,
		`CREATE TABLE ZCALLRECORD(Z_PK INTEGER PRIMARY KEY,ZUNIQUE_ID TEXT,ZDATE TIMESTAMP); INSERT INTO ZCALLRECORD VALUES(1,'secret',x'3132');`,
		`CREATE TABLE ZCALLRECORD(Z_PK INTEGER PRIMARY KEY,ZUNIQUE_ID TEXT,ZDATE TIMESTAMP); INSERT INTO ZCALLRECORD VALUES(1,'secret','secret');`,
	} {
		_, path := sourceDB(t, schema)
		err := Read(t.Context(), path, "example", func(callhistory.Observation) error { require.FailNow("invalid source must not emit"); return nil })
		require.Error(err)
		assert.NotContains(err.Error(), "secret")
	}
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	_, path := sourceDB(t, `CREATE TABLE ZCALLRECORD(Z_PK INTEGER PRIMARY KEY,ZUNIQUE_ID TEXT,ZDATE REAL); INSERT INTO ZCALLRECORD VALUES(1,'one',0),(2,'two',1);`)
	stop := errors.New("stop")
	calls := 0
	err := Read(t.Context(), path, "example", func(o callhistory.Observation) error {
		assert.Contains(string(o.Raw), `"ZADDRESS":null`)
		calls++
		return stop
	})
	require.ErrorIs(err, stop)
	assert.Equal(1, calls)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(Read(ctx, path, "example", func(callhistory.Observation) error { return nil }), context.Canceled)
	require.Error(Read(t.Context(), filepath.Join(t.TempDir(), "missing"), "example", func(callhistory.Observation) error { return nil }))
	entries, err := os.ReadDir(temp)
	require.NoError(err)
	assert.Empty(entries)
}

func TestSnapshotCancellationWhileLocked(t *testing.T) {
	require := require.New(t)
	db, path := sourceDB(t, `CREATE TABLE ZCALLRECORD(Z_PK INTEGER PRIMARY KEY,ZUNIQUE_ID TEXT,ZDATE REAL);`)

	source, err := sql.Open("sqlite3", fileDSN(path, "ro"))
	require.NoError(err)
	defer func() { _ = source.Close() }()
	conn, err := source.Conn(t.Context())
	require.NoError(err)
	defer func() { _ = conn.Close() }()
	dest, err := sql.Open("sqlite3", ":memory:")
	require.NoError(err)
	defer func() { _ = dest.Close() }()
	dc, err := dest.Conn(t.Context())
	require.NoError(err)
	defer func() { _ = dc.Close() }()
	_, err = db.Exec(`BEGIN EXCLUSIVE`)
	require.NoError(err)
	defer func() { _, _ = db.Exec(`ROLLBACK`) }()
	// Bound the blocked backup after its connections and lock are ready.
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err = backup(ctx, dc, conn)
	require.ErrorIs(err, context.DeadlineExceeded)
}

func TestFileDSNDrivePath(t *testing.T) {
	u, err := url.Parse(fileDSN("C:/Call History?#.storedata", "ro"))
	require.NoError(t, err)
	assert.Empty(t, u.Host, "a drive letter is a path, not a SQLite URI authority")
	assert.Equal(t, "/C:/Call History?#.storedata", u.Path)
}

// TIMESTAMP/BOOLEAN declarations must not turn raw integers into Unix
// timestamps or booleans before normalization.
func FuzzReadDeclaredTypes(f *testing.F) {
	f.Add(int64(0), int64(1))
	f.Add(int64(-1), int64(0))
	f.Add(int64(123456789), int64(2))
	f.Add(int64(-63113904000), int64(1))
	f.Fuzz(func(t *testing.T, seconds, answered int64) {
		require := require.New(t)
		assert := assert.New(t)
		db, path := sourceDB(t, `CREATE TABLE ZCALLRECORD(Z_PK INTEGER PRIMARY KEY,ZUNIQUE_ID TEXT,ZDATE TIMESTAMP,ZANSWERED BOOLEAN);`)
		_, err := db.Exec(`INSERT INTO ZCALLRECORD VALUES(1,'typed-call',?,?)`, seconds, answered)
		require.NoError(err)
		var got []callhistory.Observation
		err = Read(t.Context(), path, "example", func(o callhistory.Observation) error { got = append(got, o); return nil })
		if seconds <= -63113904000 || seconds >= 252423993600 {
			require.Error(err)
			assert.Empty(got)
			return
		}
		require.NoError(err)
		require.Len(got, 1)
		assert.Equal(time.Unix(seconds+978307200, 0).UTC(), got[0].OccurredAt)
		assert.Equal(answered == 1, got[0].OwnerParticipation == "joined")
		var raw map[string]jsontext.Value
		require.NoError(json.Unmarshal(got[0].Raw, &raw))
		assert.Equal(strconv.FormatInt(seconds, 10), string(raw["ZDATE"]))
		assert.Equal(strconv.FormatInt(answered, 10), string(raw["ZANSWERED"]))
	})
}
