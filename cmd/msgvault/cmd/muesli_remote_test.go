package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"go.kenn.io/msgvault/internal/activity"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/muesli"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMuesliLocalReaderToRemoteArchive(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.Analytics.AutoBuildCache = false
	cfg.Server.APIKey = "synthetic-owner-key"
	adapter := &storeAPIAdapter{store: st, config: cfg, logger: slog.New(slog.DiscardHandler)}
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, Logger: adapter.logger, OperationGate: api.NewSerialOperationGate()}).Router())
	defer server.Close()
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: cfg.Server.APIKey, AllowInsecure: true})
	require.NoError(err)
	source := meetingimport.Source{Identifier: "recorder", AccountEmail: "user@example.com"}
	registered, err := client.ImportMuesli(t.Context(), muesli.RemoteRequest{Action: "register", Source: source})
	require.NoError(err)
	path := filepath.Join(t.TempDir(), "muesli.db")
	db, err := sql.Open("sqlite3", path)
	require.NoError(err)
	defer func() { _ = db.Close() }()
	_, err = db.Exec(`CREATE TABLE meetings (id INTEGER PRIMARY KEY, title TEXT, start_time TEXT, created_at TEXT, meeting_status TEXT, raw_transcript TEXT, manual_notes TEXT, deleted_at TEXT);
 INSERT INTO meetings VALUES (42,'Planning','2026-09-01T14:00:00Z','2026-09-01 14:00:03','completed','Initial synthetic transcript','',NULL);
 INSERT INTO meetings VALUES (43,'Unfinished','2026-09-01T14:00:00Z','2026-09-01 14:00:04','processing','unfinished','',NULL)`)
	require.NoError(err)
	options := muesli.ImportOptions{Identifier: source.Identifier, AccountEmail: source.AccountEmail, DBPath: path, LockDir: t.TempDir()}
	scan := func() *muesli.ImportSummary {
		summary, err := muesli.ScanRemote(t.Context(), options, client.ImportMuesli)
		require.NoError(err)
		return summary
	}
	assert.Equal(int64(1), scan().MeetingsAdded)
	unchanged := scan()
	assert.Zero(unchanged.MeetingsUpdated)
	assert.Equal(int64(1), unchanged.SkippedInProgress)
	_, err = db.Exec(`UPDATE meetings SET raw_transcript='Edited synthetic transcript',manual_notes='Later notes' WHERE id=42`)
	require.NoError(err)
	assert.Equal(int64(1), scan().MeetingsUpdated)
	var messageID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_id=?`), registered.SourceID).Scan(&messageID))
	detail, err := st.GetMessage(messageID)
	require.NoError(err)
	assert.Contains(detail.Body, "Edited synthetic transcript")
	assert.Contains(detail.Body, "Later notes")
	_, err = db.Exec(`UPDATE meetings SET deleted_at='2026-09-02T00:00:00Z' WHERE id=42`)
	require.NoError(err)
	assert.Equal(int64(1), scan().SkippedDeleted)
	detail, err = st.GetMessage(messageID)
	require.NoError(err)
	assert.Contains(detail.Body, "Later notes")
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = client.ImportMuesli(canceled, muesli.RemoteRequest{Action: "register", Source: source})
	require.ErrorIs(err, context.Canceled)
}

func TestMuesliDuplicateContactsRemoteReview(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.Analytics.AutoBuildCache = false
	cfg.Server.APIKey = "synthetic-owner-key"
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	modes := make(chan buildCacheMode, 10)
	jobs := newCacheBuildJobs(ctx, nil, func(_ context.Context, mode buildCacheMode) error { modes <- mode; return nil })
	t.Cleanup(func() {
		cancel()
		wait, stop := context.WithTimeout(context.Background(), serveLifecycleTestTimeout)
		defer stop()
		must.True(jobs.waitContext(wait))
	})
	adapter := &storeAPIAdapter{store: st, config: cfg, cacheJobs: jobs, logger: slog.New(slog.DiscardHandler)}
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, Logger: adapter.logger, OperationGate: api.NewSerialOperationGate()}).Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: cfg.Server.APIKey, AllowInsecure: true})
	must.NoError(err)
	source := meetingimport.Source{Identifier: "recorder", AccountEmail: "user@example.com"}
	_, err = client.ImportMuesli(t.Context(), muesli.RemoteRequest{Action: "register", Source: source})
	must.NoError(err)
	root := t.TempDir()
	path := filepath.Join(root, "muesli.db")
	db, err := sql.Open("sqlite3", path)
	must.NoError(err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`CREATE TABLE meetings (id INTEGER PRIMARY KEY,title TEXT,start_time TEXT,created_at TEXT,meeting_status TEXT,raw_transcript TEXT);
 INSERT INTO meetings VALUES(42,'Planning','2026-09-01T14:00:00Z','2026-09-01 14:00:03','completed','Synthetic transcript');
 CREATE TABLE meeting_participants (meeting_id INTEGER,participant_identifier TEXT,display_name TEXT,email_address TEXT,source TEXT,insertion_order INTEGER);
 INSERT INTO meeting_participants VALUES(42,'email:attendee@example.com','Meeting Attendee','attendee@example.com','calendar',0)`)
	must.NoError(err)
	contacts := filepath.Join(root, "AddressBook")
	must.NoError(os.MkdirAll(contacts, 0o755))
	cdb, err := sql.Open("sqlite3", filepath.Join(contacts, "AddressBook-v22.abcddb"))
	must.NoError(err)
	t.Cleanup(func() { _ = cdb.Close() })
	_, err = cdb.Exec(`CREATE TABLE Z_PRIMARYKEY (Z_ENT INTEGER,Z_NAME TEXT);INSERT INTO Z_PRIMARYKEY VALUES(22,'ABCDContact');
 CREATE TABLE ZABCDRECORD (Z_PK INTEGER,Z_ENT INTEGER,ZUNIQUEID TEXT,ZLINKID TEXT);INSERT INTO ZABCDRECORD VALUES(1,22,'CARD-A:ABPerson',NULL),(2,22,'CARD-B:ABPerson',NULL);
 CREATE TABLE ZABCDEMAILADDRESS (Z_PK INTEGER,ZOWNER INTEGER,ZADDRESS TEXT,ZADDRESSNORMALIZED TEXT,ZORDERINGINDEX INTEGER);INSERT INTO ZABCDEMAILADDRESS VALUES(1,1,'attendee@example.com','attendee@example.com',0),(2,2,'attendee@example.com','attendee@example.com',0);
 CREATE TABLE ZABCDPHONENUMBER (Z_PK INTEGER,ZOWNER INTEGER,ZFULLNUMBER TEXT,ZORDERINGINDEX INTEGER);INSERT INTO ZABCDPHONENUMBER VALUES(1,1,'+16045550100',0),(2,2,'+16045550101',0)`)
	must.NoError(err)
	options := muesli.ImportOptions{Identifier: source.Identifier, AccountEmail: source.AccountEmail, DBPath: path, ContactsEnabled: true, ContactsPath: contacts, LockDir: t.TempDir()}
	scan := func() *muesli.ImportSummary {
		sum, err := muesli.ScanRemote(t.Context(), options, client.ImportMuesli)
		must.NoError(err)
		return sum
	}
	check.Equal(int64(1), scan().MeetingsAdded)
	phoneID, err := st.EnsureParticipantByPhone("+16045550100", "Existing Chat", "imessage")
	must.NoError(err)
	person, _, err := st.CreatePersonFromParticipantContext(t.Context(), phoneID)
	must.NoError(err)
	// A phone learned later creates review evidence from an unchanged meeting.
	cfg.Analytics.AutoBuildCache = true
	check.Zero(scan().MeetingsUpdated)
	must.True(jobs.waitContext(t.Context()))
	check.Empty(modes, "candidate-only writes must not start an analytics build")
	matches, err := st.ListIdentityMatchReviewsContext(t.Context(), nil, 100, 0)
	must.NoError(err)
	must.Len(matches, 1)
	reviewed, err := client.GetIdentityMatch(t.Context(), matches[0].ID)
	must.NoError(err)
	var output bytes.Buffer
	must.NoError(writeIdentityMatchDetail(&output, reviewed))
	check.Contains(output.String(), "attendee@example.com")
	check.Contains(output.String(), "+16045550100")
	check.Contains(output.String(), "historical")
	check.Zero(scan().MeetingsUpdated)
	again, err := client.GetIdentityMatch(t.Context(), reviewed.ID)
	must.NoError(err)
	check.Equal(reviewed.ReviewToken, again.ReviewToken)
	check.Len(again.Evidence, 1)
	emailID, err := st.EnsureParticipant("attendee@example.com", "", "example.com")
	must.NoError(err)
	members, err := st.ClusterMembers(phoneID)
	must.NoError(err)
	check.NotContains(members, emailID)
	cfg.Analytics.AutoBuildCache = false
	must.NotNil(reviewed.ReviewToken)
	accepted, err := client.AcceptIdentityMatch(t.Context(), reviewed.ID, *reviewed.ReviewToken, nil)
	must.NoError(err)
	check.Equal("accepted", accepted.Candidate.State)
	members, err = st.ClusterMembers(phoneID)
	must.NoError(err)
	check.Contains(members, emailID)
	projector, err := activity.NewProjector(st, activity.Options{Timezone: "UTC", BatchSize: 10, MaxDirectCounterparts: 25})
	must.NoError(err)
	_, err = projector.RunOnce(t.Context())
	must.NoError(err)
	var attributed int
	must.NoError(st.DB().QueryRow(st.Rebind(`SELECT count(*) FROM activity_event_persons WHERE person_id = ?`), person.ID).Scan(&attributed))
	check.Equal(1, attributed)
	check.Zero(scan().MeetingsUpdated)
	t.Run("daemon drops over-budget review evidence without rejecting", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		// Archive resolved evidence that later becomes ambiguous on the recorder.
		_, err := muesli.ScanRemote(t.Context(), options, func(ctx context.Context, r muesli.RemoteRequest) (muesli.RemoteResult, error) {
			p := &r.Meeting.Participants[0]
			p.ContactReviewPhones = nil
			p.Resolution = "resolved"
			for i := range 30 {
				p.Phones = append(p.Phones, fmt.Sprintf("+1604555%04d", 100+i))
			}
			return client.ImportMuesli(ctx, r)
		})
		require.NoError(err)
		registeredSource, err := st.GetSourceByTypeAndIdentifier("muesli", source.Identifier)
		require.NoError(err)
		var messageID int64
		require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_id = ?`), registeredSource.ID).Scan(&messageID))
		before, err := st.GetMessageRaw(messageID)
		require.NoError(err)
		_, err = cdb.Exec(`DELETE FROM ZABCDPHONENUMBER`)
		require.NoError(err)
		for i := 30; i < 60; i++ {
			_, err = cdb.Exec(`INSERT INTO ZABCDPHONENUMBER VALUES(?,1,?,?)`, i, fmt.Sprintf("+1604555%04d", 100+i), i)
			require.NoError(err)
		}
		_, err = db.Exec(`INSERT INTO meetings VALUES(43,'Later meeting','2026-09-02T14:00:00Z','2026-09-02 14:00:03','completed','Later synthetic transcript')`)
		require.NoError(err)
		for _, wantAdded := range []int64{1, 0} {
			sum, err := muesli.ScanRemote(t.Context(), options, client.ImportMuesli)
			require.NoError(err)
			assert.Zero(sum.Errors)
			assert.Equal(wantAdded, sum.MeetingsAdded)
		}
		after, err := st.GetMessageRaw(messageID)
		require.NoError(err)
		assert.Equal(before, after, "carried evidence stays and over-budget review phones are not archived")
	})
}
