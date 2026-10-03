package cmd

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/muesli"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMuesliScheduledUnchangedDoesNotRefresh(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource(muesli.SourceType, "recorder")
	require.NoError(err)
	path := filepath.Join(t.TempDir(), "muesli.db")
	db, err := sql.Open("sqlite3", path)
	require.NoError(err)
	defer func() { _ = db.Close() }()
	_, err = db.Exec(`CREATE TABLE meetings (id INTEGER PRIMARY KEY,title TEXT,start_time TEXT,created_at TEXT,raw_transcript TEXT);INSERT INTO meetings VALUES(42,'Planning','2026-09-01T14:00:00Z','2026-09-01 14:00:03','Synthetic transcript')`)
	require.NoError(err)
	off := false
	source := config.MuesliSource{Identifier: "recorder", AccountEmail: "user@example.com", DBPath: path, Contacts: &off}
	original := rebuildMuesliCacheAfterScheduledSync
	t.Cleanup(func() { rebuildMuesliCacheAfterScheduledSync = original })
	calls := 0
	rebuildMuesliCacheAfterScheduledSync = func(context.Context, string) error { calls++; return nil }
	require.NoError(runConfiguredMuesliSync(t.Context(), st, source))
	assert.Equal(1, calls)
	require.NoError(runConfiguredMuesliSync(t.Context(), st, source))
	assert.Equal(1, calls, "unchanged scheduled import must not refresh")
}

func TestMuesliRemoteDefaultUnchangedSkipAndExplicitRefresh(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.Analytics.AutoBuildCache = false
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	modes := make(chan buildCacheMode, 2)
	jobs := newCacheBuildJobs(ctx, nil, func(_ context.Context, mode buildCacheMode) error { modes <- mode; return nil })
	t.Cleanup(func() {
		cancel()
		wait, stop := context.WithTimeout(context.Background(), serveLifecycleTestTimeout)
		defer stop()
		require.True(jobs.waitContext(wait))
	})
	adapter := &storeAPIAdapter{store: st, config: cfg, cacheJobs: jobs}
	source := meetingimport.Source{Identifier: "recorder", AccountEmail: "user@example.com"}
	_, err := adapter.ImportMuesli(t.Context(), muesli.RemoteRequest{Action: "register", Source: source})
	require.NoError(err)
	request := muesli.RemoteRequest{Action: "upsert", Source: source, Meeting: muesli.NewRemoteMeeting(muesli.Meeting{ID: 42, Title: "Planning", Status: "completed", CreatedAt: "2026-09-01 14:00:03", StartTime: "2026-09-01T14:00:00Z", RawTranscript: "Synthetic transcript"})}
	created, err := adapter.ImportMuesli(t.Context(), request)
	require.NoError(err)
	assert.True(created.Changed)
	unchanged, err := adapter.ImportMuesli(t.Context(), request)
	require.NoError(err)
	assert.False(unchanged.Changed)
	require.True(jobs.waitContext(t.Context()))
	assert.Empty(modes, "automatic cache refresh is disabled")
	skipped := request
	skipped.NoBuildCache = true
	skipped.Meeting = muesli.NewRemoteMeeting(muesli.Meeting{ID: 42, Title: "Changed", Status: "completed", CreatedAt: "2026-09-01 14:00:03", StartTime: "2026-09-01T14:00:00Z", RawTranscript: "Synthetic transcript"})
	_, err = adapter.ImportMuesli(t.Context(), skipped)
	require.NoError(err)
	assert.Empty(modes)
	refreshed, err := adapter.ImportMuesli(t.Context(), muesli.RemoteRequest{Action: "refresh", Source: source, BuildCache: true})
	require.NoError(err)
	assert.Equal("refreshed", refreshed.Status)
	wait, stop := context.WithTimeout(t.Context(), serveLifecycleTestTimeout)
	defer stop()
	require.True(jobs.waitContext(wait))
	select {
	case mode := <-modes:
		assert.Equal(buildCacheModeAuto, mode)
	default:
		require.FailNow("explicit refresh did not queue a build")
	}
}

func TestMuesliRemoteUnchangedPreservesDeferredCacheRefresh(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.Analytics.AutoBuildCache = true
	adapter := &storeAPIAdapter{store: st, config: cfg}
	requested := make(chan string, 2)
	var refresher *backgroundCacheRefresher
	refresher = newBackgroundCacheRefresher(adapter.invocationContext(t.Context()), func(_ context.Context, id string) error {
		refresher.RequestAfter(time.Hour, id)
		requested <- id
		return nil
	}, nil)
	previous := daemonCacheRefresher
	daemonCacheRefresher = refresher
	t.Cleanup(func() { daemonCacheRefresher = previous; require.NoError(refresher.Shutdown(context.Background())) })
	source := meetingimport.Source{Identifier: "recorder", AccountEmail: "user@example.com"}
	_, err := adapter.ImportMuesli(t.Context(), muesli.RemoteRequest{Action: "register", Source: source})
	require.NoError(err)
	request := muesli.RemoteRequest{Action: "upsert", Source: source, Meeting: muesli.NewRemoteMeeting(muesli.Meeting{ID: 42, Title: "Planning", Status: "completed", CreatedAt: "2026-09-01 14:00:03", StartTime: "2026-09-01T14:00:00Z", RawTranscript: "Synthetic transcript"})}
	_, err = adapter.ImportMuesli(t.Context(), request)
	require.NoError(err)
	select {
	case <-requested:
	case <-time.After(serveLifecycleTestTimeout):
		require.FailNow("changed meeting did not request a background refresh")
	}
	refresher.mu.Lock()
	delayed := refresher.delayed
	refresher.mu.Unlock()
	require.NotNil(delayed)
	unchanged, err := adapter.ImportMuesli(t.Context(), request)
	require.NoError(err)
	assert.False(unchanged.Changed)
	refresher.mu.Lock()
	remaining := refresher.delayed
	refresher.mu.Unlock()
	assert.Same(delayed, remaining, "unchanged scan must leave the scheduled retry intact")
}

func TestMuesliScheduledIdentityOnlyChangeRefreshes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource(muesli.SourceType, "recorder")
	require.NoError(err)
	root := t.TempDir()
	path := filepath.Join(root, "muesli.db")
	db, err := sql.Open("sqlite3", path)
	require.NoError(err)
	defer func() { _ = db.Close() }()
	_, err = db.Exec(`CREATE TABLE meetings (id INTEGER PRIMARY KEY,title TEXT,start_time TEXT,created_at TEXT,raw_transcript TEXT);
 INSERT INTO meetings VALUES(42,'Planning','2026-09-01T14:00:00Z','2026-09-01 14:00:03','Synthetic transcript');
 CREATE TABLE meeting_participants(meeting_id INTEGER,participant_identifier TEXT,display_name TEXT,email_address TEXT,insertion_order INTEGER,source TEXT);
 INSERT INTO meeting_participants VALUES(42,'contact:CARD-A:ABPerson','Alex Example','',0,'calendar')`)
	require.NoError(err)
	contactsPath := filepath.Join(root, "AddressBook")
	require.NoError(os.MkdirAll(contactsPath, 0700))
	contacts, err := sql.Open("sqlite3", filepath.Join(contactsPath, "AddressBook-v22.abcddb"))
	require.NoError(err)
	defer func() { _ = contacts.Close() }()
	_, err = contacts.Exec(`CREATE TABLE Z_PRIMARYKEY(Z_ENT INTEGER,Z_NAME TEXT); INSERT INTO Z_PRIMARYKEY VALUES(22,'ABCDContact');
 CREATE TABLE ZABCDRECORD(Z_PK INTEGER PRIMARY KEY,Z_ENT INTEGER,ZUNIQUEID TEXT,ZLINKID TEXT,ZFIRSTNAME TEXT,ZLASTNAME TEXT,ZORGANIZATION TEXT);
 INSERT INTO ZABCDRECORD VALUES(1,22,'CARD-A:ABPerson',NULL,'Alex','Example',''),(2,22,'CARD-B:ABPerson',NULL,'Other','Example','');
 CREATE TABLE ZABCDEMAILADDRESS(Z_PK INTEGER PRIMARY KEY,ZOWNER INTEGER,ZADDRESS TEXT,ZADDRESSNORMALIZED TEXT,ZORDERINGINDEX INTEGER);
 INSERT INTO ZABCDEMAILADDRESS VALUES(1,1,'alex@example.com',NULL,0);
 CREATE TABLE ZABCDPHONENUMBER(Z_PK INTEGER PRIMARY KEY,ZOWNER INTEGER,ZFULLNUMBER TEXT,ZORDERINGINDEX INTEGER);
 INSERT INTO ZABCDPHONENUMBER VALUES(1,1,'+16045550100',0),(2,2,'+16045550100',0)`)
	require.NoError(err)
	source := config.MuesliSource{Identifier: "recorder", AccountEmail: "user@example.com", DBPath: path, ContactsPath: contactsPath}
	original := rebuildMuesliCacheAfterScheduledSync
	t.Cleanup(func() { rebuildMuesliCacheAfterScheduledSync = original })
	calls := 0
	rebuildMuesliCacheAfterScheduledSync = func(context.Context, string) error { calls++; return nil }
	require.NoError(runConfiguredMuesliSync(t.Context(), st, source))
	assert.Equal(1, calls)
	_, err = contacts.Exec(`DELETE FROM ZABCDPHONENUMBER WHERE ZOWNER=2`)
	require.NoError(err)
	require.NoError(runConfiguredMuesliSync(t.Context(), st, source))
	assert.Equal(2, calls, "identity-only links must refresh the scheduled cache")
	require.NoError(runConfiguredMuesliSync(t.Context(), st, source))
	assert.Equal(2, calls, "settled identities are unchanged")
}
