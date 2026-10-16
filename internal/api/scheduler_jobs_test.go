package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/whatsapp"
)

func TestCardDAVSchedulerJobNameIsStable(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "carddav", CardDAVJobName)
}

func TestPlaudSchedulerJobName(t *testing.T) {
	name, ok := SchedulerJobNameForSource("plaud", "work")
	assert.True(t, ok)
	assert.Equal(t, "plaud:work", name)
}

func TestPlaudDaemonCLIAllowlist(t *testing.T) {
	assert := assert.New(t)
	assert.True(cliRunCommandAllowed([]string{"add-plaud", "work"}))
	assert.True(cliRunCommandAllowed([]string{"sync-plaud", "work", "--limit", "5"}))
	assert.True(cliRunCommandAllowed([]string{"sync-plaud", "--probe"}))
	for _, args := range [][]string{{"add-twilio"}, {"add-twilio", "account"}, {"sync-twilio"}, {"sync-twilio", "account", "--full"}} {
		assert.True(cliRunCommandAllowed(args), "Twilio command must be runnable via daemon: %v", args)
	}
}

// TestTriggerAppleImportRunsDetachedFromRequest proves that cancelling the
// triggering HTTP request does not cancel the daemon-owned run, and that
// repeated triggers while it runs start no second run.
func TestTriggerAppleImportRunsDetachedFromRequest(t *testing.T) {
	require := require.New(t)
	sched := scheduler.New(func(context.Context, string) error { return nil })
	started := make(chan struct{})
	release := make(chan struct{})
	second := make(chan struct{})
	var runs, cancelled atomic.Int32
	var startOnce sync.Once
	require.NoError(sched.AddJob(scheduler.Job{
		Name:     IMessageJobName,
		Schedule: "0 0 1 1 *",
		Run: func(ctx context.Context) error {
			secondRun := runs.Add(1) == 2
			startOnce.Do(func() { close(started) })
			select {
			case <-ctx.Done():
				cancelled.Add(1)
			case <-release:
			}
			if secondRun {
				close(second)
			}
			return nil
		},
	}))
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource("apple_messages", "local")
	require.NoError(err)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, sched, testLogger())

	reqCtx, cancelReq := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sync/local?source_type=apple_messages", nil).WithContext(reqCtx)
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	require.Equal(http.StatusAccepted, resp.Code, resp.Body.String())
	<-started
	cancelReq()

	for range 5 {
		resp = servePOSTTestRequest(srv, "/api/v1/sync/local?source_type=apple_messages")
		require.Equal(http.StatusAccepted, resp.Code, resp.Body.String())
	}
	require.Equal(int32(1), runs.Load(), "triggers during a run must not start a second concurrent run")
	close(release)
	<-second
	<-sched.Stop().Done()
	require.Zero(cancelled.Load(), "run must outlive the triggering request")
	require.Equal(int32(2), runs.Load(), "the five triggers coalesce into exactly one follow-up run")
}

func TestAppleImportSchedulingSelectsOnlyImportedSource(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	local, err := st.GetOrCreateSource("apple_messages", "local")
	require.NoError(err)
	legacy, err := st.GetOrCreateSource("apple_messages", "+15551234567")
	require.NoError(err)
	selected, err := st.GetOrCreateSource("apple_messages", "+15557654321")
	require.NoError(err)
	sched := scheduler.New(func(context.Context, string) error { return nil })
	var runs atomic.Int32
	require.NoError(sched.AddJob(scheduler.Job{Name: IMessageJobName, Schedule: "0 0 1 1 *", Run: func(context.Context) error { runs.Add(1); return nil }}))
	defer func() { <-sched.Stop().Done() }()
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, sched, testLogger())
	for _, source := range []*store.Source{local, legacy} {
		status, err := srv.sourceStatus(context.Background(), st, source)
		require.NoError(err)
		assert.False(status.Scheduled)
		assert.False(status.CanSync)
		assert.Equal("source_not_schedulable", status.SyncUnavailableReason)
		resp := servePOSTTestRequest(srv, "/api/v1/sync/"+source.Identifier+"?source_type=apple_messages")
		assert.Equal(http.StatusBadRequest, resp.Code, resp.Body.String())
	}
	assert.Zero(runs.Load(), "an unselected source must not start the singleton import")
	status, err := srv.sourceStatus(context.Background(), st, selected)
	require.NoError(err)
	assert.True(status.Scheduled)
	assert.True(status.CanSync)
	resp := servePOSTTestRequest(srv, "/api/v1/sync/"+selected.Identifier+"?source_type=apple_messages")
	assert.Equal(http.StatusAccepted, resp.Code, resp.Body.String())
}

func TestTriggerFirstIMessageImportFromEmptyArchive(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	sched := scheduler.New(func(context.Context, string) error { return nil })
	started := make(chan struct{})
	require.NoError(sched.AddJob(scheduler.Job{Name: IMessageJobName, Schedule: "0 0 1 1 *", Run: func(context.Context) error { close(started); return nil }}))
	defer func() { <-sched.Stop().Done() }()
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, sched, testLogger())
	resp := servePOSTTestRequest(srv, "/api/v1/sync/local?source_type=apple_messages")
	require.Equal(http.StatusAccepted, resp.Code, resp.Body.String())
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		require.FailNow("scheduled first import did not start")
	}
	sources, err := st.ListSources("apple_messages")
	require.NoError(err)
	assert.Empty(t, sources, "admission does not create an archive source")
	resp = servePOSTTestRequest(srv, "/api/v1/sync/other?source_type=apple_messages")
	assert.Equal(t, http.StatusBadRequest, resp.Code, resp.Body.String())
}

// createTicketChatStorage writes a minimal WhatsApp for Mac ChatStorage with
// one direct chat and one message.
func createTicketChatStorage(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ChatStorage.sqlite")
	execTicketChatStorage(t, path, `
		CREATE TABLE ZWACHATSESSION (Z_PK INTEGER PRIMARY KEY, ZCONTACTJID TEXT,
			ZPARTNERNAME TEXT, ZSESSIONTYPE INTEGER, ZLASTMESSAGEDATE TIMESTAMP);
		CREATE TABLE ZWAGROUPMEMBER (Z_PK INTEGER PRIMARY KEY, ZCHATSESSION INTEGER,
			ZMEMBERJID TEXT, ZCONTACTNAME TEXT, ZFIRSTNAME TEXT, ZISADMIN INTEGER);
		CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY, ZCHATSESSION INTEGER,
			ZGROUPMEMBER INTEGER, ZSTANZAID TEXT, ZISFROMME INTEGER,
			ZMESSAGEDATE TIMESTAMP, ZTEXT TEXT, ZMESSAGETYPE INTEGER, ZFROMJID TEXT);
		INSERT INTO ZWACHATSESSION VALUES (1, '15555550101@s.whatsapp.net', 'Alice Test', 0, 700000000);
		INSERT INTO ZWAMESSAGE VALUES (1, 1, NULL, 'first', 0, 700000000, 'first text', 0, '15555550101@s.whatsapp.net');
	`)
	return path
}

func execTicketChatStorage(t *testing.T, path, statements string) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = db.Exec(statements)
	require.NoError(t, err)
	require.NoError(t, db.Close())
}

func archivedSourceMessage(t *testing.T, st *store.Store, sourceMessageID string) bool {
	t.Helper()
	var n int
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), sourceMessageID).Scan(&n))
	return n > 0
}

func getSyncTicket(t *testing.T, srv *Server, target string) (int, SyncTicketResponse) {
	t.Helper()
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, target, nil))
	var body SyncTicketResponse
	if resp.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &body), resp.Body.String())
	}
	return resp.Code, body
}

func triggerSyncTicket(t *testing.T, srv *Server, target string) TriggerSyncResponse {
	t.Helper()
	resp := servePOSTTestRequest(srv, target)
	require.Equal(t, http.StatusAccepted, resp.Code, resp.Body.String())
	var body TriggerSyncResponse
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &body))
	require.NotEmpty(t, body.Ticket, "a generic trigger returns a ticket")
	return body
}

// TestSyncTicketCoversMessageWrittenDuringRun drives the real Apple WhatsApp
// importer through the HTTP trigger and ticket endpoints. A message written
// to ChatStorage after the running import read it, then requested, is in the
// archive once the request's ticket completes.
func TestSyncTicketCoversMessageWrittenDuringRun(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	const phone = "+15555550100"
	chatStorage := createTicketChatStorage(t)
	st := testutil.NewTestStore(t)

	proceed := make(chan struct{}, 2) // lets a run read ChatStorage
	imported := make(chan struct{}, 2)
	release := make(chan struct{}, 2)
	sched := scheduler.New(func(context.Context, string) error { return nil })
	defer func() { <-sched.Stop().Done() }()
	require.NoError(sched.AddJob(scheduler.Job{
		Name:     WhatsAppAppleJobName(phone),
		Schedule: "0 0 1 1 *",
		Run: func(ctx context.Context) error {
			<-proceed
			_, err := whatsapp.NewImporter(st, nil).Import(ctx, chatStorage, whatsapp.ImportOptions{
				Phone: phone, DisplayName: "Test Owner",
			})
			// The run has read ChatStorage; hold it open until released.
			imported <- struct{}{}
			<-release
			return err
		},
	}))
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, nil, sched, testLogger())
	trigger := "/api/v1/sync/" + phone + "?source_type=whatsapp"
	ticketURL := func(ticket string) string {
		return "/api/v1/sync/" + phone + "/tickets/" + ticket + "?source_type=whatsapp&wait=30s"
	}

	proceed <- struct{}{}
	first := triggerSyncTicket(t, srv, trigger)
	assert.Equal(string(scheduler.JobStarted), first.Disposition)
	<-imported
	execTicketChatStorage(t, chatStorage, `
		INSERT INTO ZWAMESSAGE VALUES (2, 1, NULL, 'late', 0, 700000100, 'late text', 0, '15555550101@s.whatsapp.net');
		UPDATE ZWACHATSESSION SET ZLASTMESSAGEDATE = 700000100 WHERE Z_PK = 1;
	`)
	late := triggerSyncTicket(t, srv, trigger)
	assert.Equal(string(scheduler.JobPending), late.Disposition)

	release <- struct{}{}
	code, status := getSyncTicket(t, srv, ticketURL(first.Ticket))
	require.Equal(http.StatusOK, code)
	assert.Equal(scheduler.TicketCompleted, status.State)
	assert.False(archivedSourceMessage(t, st, "late"), "the run in progress read ChatStorage before the late message")

	// The rerun holds the gate but has not read ChatStorage yet: the late
	// ticket must not be answered.
	code, status = getSyncTicket(t, srv, "/api/v1/sync/"+phone+"/tickets/"+late.Ticket+"?source_type=whatsapp&wait=200ms")
	require.Equal(http.StatusOK, code)
	assert.False(status.State.Terminal(), "late ticket answered before any run read the late message: %s", status.State)

	proceed <- struct{}{}
	<-imported
	release <- struct{}{}
	code, status = getSyncTicket(t, srv, ticketURL(late.Ticket))
	require.Equal(http.StatusOK, code)
	require.Equal(scheduler.TicketCompleted, status.State)
	assert.True(archivedSourceMessage(t, st, "late"), "the late message is archived when its ticket completes")
}

// TestSyncTicketEndpointErrors covers the ticket endpoint's refusals and the
// abandoned answer after the scheduler stops.
func TestSyncTicketEndpointErrors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource("apple_messages", "local")
	require.NoError(err)
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	sched := scheduler.New(func(context.Context, string) error { return nil })
	require.NoError(sched.AddJob(scheduler.Job{
		Name:     IMessageJobName,
		Schedule: "0 0 1 1 *",
		Run: func(ctx context.Context) error {
			started <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil
		},
	}))
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, sched, testLogger())
	running := triggerSyncTicket(t, srv, "/api/v1/sync/local?source_type=apple_messages")
	// A run covers every ticket issued before it takes the work gate, so the
	// second request is queued only once the first run is executing.
	<-started
	queued := triggerSyncTicket(t, srv, "/api/v1/sync/local?source_type=apple_messages")

	code, status := getSyncTicket(t, srv, "/api/v1/sync/local/tickets/"+queued.Ticket+"?source_type=apple_messages")
	assert.Equal(http.StatusOK, code, "no wait answers at once")
	assert.Equal(scheduler.TicketQueued, status.State)

	code, _ = getSyncTicket(t, srv, "/api/v1/sync/local/tickets/nonsense?source_type=apple_messages")
	assert.Equal(http.StatusBadRequest, code)
	code, _ = getSyncTicket(t, srv, "/api/v1/sync/local/tickets/"+queued.Ticket+"?source_type=apple_messages&wait=soon")
	assert.Equal(http.StatusBadRequest, code)
	code, _ = getSyncTicket(t, srv, "/api/v1/sync/a@example.com/tickets/"+queued.Ticket)
	assert.Equal(http.StatusBadRequest, code, "account syncs issue no tickets")
	epoch, _, _ := strings.Cut(queued.Ticket, "-")
	code, _ = getSyncTicket(t, srv, "/api/v1/sync/local/tickets/"+epoch+"-99?source_type=apple_messages")
	assert.Equal(http.StatusNotFound, code)

	waited := make(chan *httptest.ResponseRecorder, 2)
	for _, ticket := range []string{running.Ticket, queued.Ticket} {
		go func() {
			resp := httptest.NewRecorder()
			srv.Router().ServeHTTP(resp, httptest.NewRequest(http.MethodGet,
				"/api/v1/sync/local/tickets/"+ticket+"?source_type=apple_messages&wait=30s", nil))
			waited <- resp
		}()
	}
	<-sched.Stop().Done()
	close(release)
	for range 2 {
		resp := <-waited
		require.Equal(http.StatusOK, resp.Code, resp.Body.String())
		var status SyncTicketResponse
		require.NoError(json.Unmarshal(resp.Body.Bytes(), &status))
		assert.Equal(scheduler.TicketAbandoned, status.State, "a waiter is released when the scheduler stops")
	}
}
