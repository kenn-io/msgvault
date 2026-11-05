package sync

import (
	"context"
	"errors"
	"net/url"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// These fixtures follow MSGVAULT_TEST_DB, unlike newTestEnv's SQLite fixtures.
func acquisitionDurabilityEnv(t *testing.T) *TestEnv {
	t.Helper()
	st := testutil.NewTestStore(t)
	mock := gmail.NewMockAPI()
	mock.Profile = &gmail.Profile{EmailAddress: testEmail, HistoryID: 1000}
	return &TestEnv{Store: st, Mock: mock, Syncer: New(&gmailLabelSnapshotAPI{MockAPI: mock}, st, nil), Context: t.Context()}
}

func reopenAcquisitionStore(t *testing.T, e *TestEnv) {
	t.Helper()
	var location string
	if e.Store.IsPostgreSQL() {
		var database, schema string
		require.NoError(t, e.Store.DB().QueryRow("SELECT current_database(), current_schema()").Scan(&database, &schema))
		u, err := url.Parse(os.Getenv("MSGVAULT_TEST_DB"))
		require.NoError(t, err)
		u.Path = "/" + database
		params := u.Query()
		params.Set("search_path", schema)
		u.RawQuery = params.Encode()
		location = u.String()
	} else {
		var sequence int
		var name string
		require.NoError(t, e.Store.DB().QueryRow("PRAGMA database_list").Scan(&sequence, &name, &location))
	}
	require.NoError(t, e.Store.Close())
	st, err := store.OpenForTest(location)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, st.Close()) })
	e.Store, e.Syncer = st, New(&gmailLabelSnapshotAPI{MockAPI: e.Mock}, st, nil)
}

// Reject only the selected write; checkpoints and cursor publication remain
// writable. No transaction is held across the provider calls under test.
func acquisitionWriteFault(t *testing.T, e *TestEnv, table, event, condition string) func() {
	t.Helper()
	name := "f1_reject_" + table
	if e.Store.IsPostgreSQL() {
		_, err := e.Store.DB().Exec("CREATE FUNCTION " + name + `() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN RAISE EXCEPTION 'synthetic acquisition write failure'; END $$`)
		require.NoError(t, err)
		_, err = e.Store.DB().Exec("CREATE TRIGGER " + name + " BEFORE " + event + " ON " + table +
			" FOR EACH ROW WHEN (" + condition + ") EXECUTE FUNCTION " + name + "()")
		require.NoError(t, err)
	} else {
		_, err := e.Store.DB().Exec("CREATE TRIGGER " + name + " BEFORE " + event + " ON " + table +
			" WHEN " + condition + " BEGIN SELECT RAISE(ABORT, 'synthetic acquisition write failure'); END")
		require.NoError(t, err)
	}
	// Use the fixture's current Store so the fault can be removed after reopen.
	return func() {
		query := "DROP TRIGGER " + name
		if e.Store.IsPostgreSQL() {
			query += " ON " + table
		}
		_, err := e.Store.DB().Exec(query)
		require.NoError(t, err)
		if e.Store.IsPostgreSQL() {
			_, err = e.Store.DB().Exec("DROP FUNCTION " + name + "()")
			require.NoError(t, err)
		}
	}
}

type acquisitionDurabilityAPI struct {
	gmailLabelSnapshotAPI

	catchup       bool
	historyPages  bool
	profiles      int
	listTokens    []string
	historyTokens []string
}

func (a *acquisitionDurabilityAPI) GetProfile(ctx context.Context) (*gmail.Profile, error) {
	p, err := a.MockAPI.GetProfile(ctx)
	a.profiles++
	if err == nil && a.catchup && a.profiles == 1 {
		pinned := *p
		pinned.HistoryID = 1000 // Enumeration pins H before the later History event.
		p = &pinned
	}
	return p, err
}

func (a *acquisitionDurabilityAPI) ListMessages(ctx context.Context, query, token string) (*gmail.MessageListResponse, error) {
	a.listTokens = append(a.listTokens, token)
	return a.MockAPI.ListMessages(ctx, query, token)
}

func (a *acquisitionDurabilityAPI) ListHistory(ctx context.Context, start uint64, token string) (*gmail.HistoryResponse, error) {
	a.historyTokens = append(a.historyTokens, token)
	r, err := a.MockAPI.ListHistory(ctx, start, token)
	if err == nil && a.historyPages && token == "" {
		r.History = []gmail.HistoryRecord{historyAdded("good")}
		r.NextPageToken = "history_1"
	}
	return r, err
}

func acquisitionPrimaryFault(t *testing.T, e *TestEnv, kind string) func() {
	t.Helper()
	switch kind {
	case "ingest":
		return acquisitionWriteFault(t, e, "messages", "INSERT", "NEW.source_message_id = 'lost'")
	default:
		e.Mock.GetMessageError["lost"] = errors.New("synthetic acquisition fetch failure")
		return func() { delete(e.Mock.GetMessageError, "lost") }
	}
}

func assertAcquisitionPresent(t *testing.T, e *TestEnv, ids ...string) {
	t.Helper()
	source := gmailCoverageSource(t, e)
	existing, err := e.Store.MessageExistsBatch(source.ID, ids)
	require.NoError(t, err)
	for _, id := range ids {
		assert.Contains(t, existing, id)
	}
}

func TestAcquisitionFailureDurabilityIncremental(t *testing.T) {
	for _, route := range []string{"history", "label", "replay", "catchup"} {
		for _, kind := range []string{"fetch", "ingest"} {
			for _, rejectRecord := range []bool{false, true} {
				name := route + "/" + kind + "/durable"
				if rejectRecord {
					name = route + "/" + kind + "/rejected"
				}
				t.Run(name, func(t *testing.T) {
					require := require.New(t)
					assert := assert.New(t)

					e := acquisitionDurabilityEnv(t)
					source := e.CreateSourceWithHistory(t, "1000")
					seedMessages(e, 2, 1001, "good", "lost")
					e.SetHistory(1001, historyAdded("lost"))
					if route == "label" {
						e.SetHistory(1001, historyLabelAdded("lost", "INBOX"))
					}
					if route == "replay" {
						e.Mock.GetMessageError["lost"] = errors.New("synthetic initial fetch failure")
						e.SetHistory(1001, historyAdded("good"), historyAdded("lost"))
						runIncrementalSync(t, e)
						delete(e.Mock.GetMessageError, "lost")
						e.SetHistory(1001) // Idle replay, with no new provider event.
						source = gmailCoverageSource(t, e)
					}
					api := &acquisitionDurabilityAPI{MockAPI: e.Mock, catchup: route == "catchup", historyPages: route == "history" || route == "label"}
					e.Syncer = New(api, e.Store, nil)
					if route == "catchup" {
						e.Mock.MessagePages = [][]string{{"good"}} // lost exists only in History.
					}
					heal := acquisitionPrimaryFault(t, e, kind)
					var allowRecord func()
					if rejectRecord {
						allowRecord = acquisitionWriteFault(t, e, "sync_run_items", "INSERT", "NEW.source_message_id = 'lost' AND NEW.status = 'error'")
					}
					var summary *gmail.SyncSummary
					var err error
					if route == "catchup" {
						summary, err = e.Syncer.RecoverExpiredHistory(t.Context(), source)
					} else {
						summary, err = e.Syncer.Incremental(t.Context(), source)
					}
					if route == "catchup" {
						assert.Equal([]string{""}, api.historyTokens, "exercise History after the pinned enumeration")
					}
					if rejectRecord {
						require.ErrorIs(err, errAcquisitionFailureRecord)
						require.ErrorContains(err, "synthetic acquisition write failure")
						run, readErr := e.Store.GetLatestSync(source.ID)
						require.NoError(readErr)
						assert.Equal(store.SyncStatusFailed, run.Status)
						assert.Equal(source.SyncCursor.String, gmailCoverageSource(t, e).SyncCursor.String)
						if route == "history" || route == "label" {
							assert.Equal("history_1", run.CursorBefore.String, "failed page must not replace the previous checkpoint")
						}
						if route == "catchup" {
							cp, err := decodeRecoveryCheckpoint(run.CursorBefore.String)
							require.NoError(err)
							assert.Equal(recoveryCatchup, cp.Phase)
							assert.Equal("1000", run.CursorAfter.String, "preserve the pinned handoff")
						}
					} else {
						require.NoError(err)
						assert.Equal(int64(1), summary.Errors)
						assert.Equal("1001", gmailCoverageSource(t, e).SyncCursor.String)
					}
					assertAcquisitionPresent(t, e, "good")
					missing, readErr := e.Store.MessageExistsBatch(source.ID, []string{"lost"})
					require.NoError(readErr)
					assert.Empty(missing, "exercise a failed acquisition, not a metadata refresh")
					reopenAcquisitionStore(t, e)
					if route == "replay" || !rejectRecord {
						items, readErr := e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
						require.NoError(readErr)
						require.Len(items, 1, "retry debt must survive the run and restart")
						assert.Equal("lost", items[0].SourceMessageID)
					}
					if allowRecord != nil {
						allowRecord()
					}
					heal()
					if !rejectRecord {
						e.SetHistory(1001) // Only durable debt can recover lost now.
					}
					// No new event is introduced: failed History reads from its old
					// cursor, recovery resumes H, and idle replay uses its old debt.
					lists := e.Mock.ListMessagesCalls
					_, err = e.Syncer.Incremental(t.Context(), gmailCoverageSource(t, e))
					require.NoError(err)
					if route == "catchup" {
						assert.Equal(lists, e.Mock.ListMessagesCalls, "catch-up resumes without re-enumeration")
					}
					reopenAcquisitionStore(t, e)
					assertAcquisitionPresent(t, e, "good", "lost")
				})
			}
		}
	}
}

func TestAcquisitionFailureDurabilityFullCheckpoint(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		for _, kind := range []string{"fetch", "ingest"} {
			t.Run(map[bool]string{false: "full", true: "recovery"}[recovery]+"/"+kind, func(t *testing.T) {
				require := require.New(t)
				assert := assert.New(t)

				e := acquisitionDurabilityEnv(t)
				source := e.CreateSourceWithHistory(t, "999")
				seedMessages(e, 3, 1000, "good", "lost", "tail")
				e.Mock.MessagePages = [][]string{{"good"}, {"lost"}, {"tail"}}
				api := &acquisitionDurabilityAPI{MockAPI: e.Mock}
				e.Syncer = New(api, e.Store, nil)
				heal := acquisitionPrimaryFault(t, e, kind)
				allowRecord := acquisitionWriteFault(t, e, "sync_run_items", "INSERT", "NEW.source_message_id = 'lost' AND NEW.status = 'error'")
				var err error
				if recovery {
					_, err = e.Syncer.RecoverExpiredHistory(t.Context(), source)
				} else {
					_, err = e.Syncer.Full(t.Context(), testEmail)
				}
				require.Error(err)
				require.ErrorIs(err, errAcquisitionFailureRecord)
				assert.Equal([]string{"", "page_1"}, api.listTokens)
				assert.Empty(api.historyTokens, "must not reach catch-up after an unrecorded acquisition")
				run, err := e.Store.GetLatestSync(source.ID)
				require.NoError(err)
				assert.Equal(store.SyncStatusFailed, run.Status)
				if recovery {
					cp, err := decodeRecoveryCheckpoint(run.CursorBefore.String)
					require.NoError(err)
					assert.Equal(recoveryEnumerate, cp.Phase)
					assert.Equal("page_1", cp.PageToken)
				} else {
					assert.Equal("page_1", run.CursorBefore.String)
				}
				assert.Equal("999", gmailCoverageSource(t, e).SyncCursor.String)
				assertAcquisitionPresent(t, e, "good")
				reopenAcquisitionStore(t, e)
				allowRecord()
				heal()
				api = &acquisitionDurabilityAPI{MockAPI: e.Mock}
				e.Syncer = New(api, e.Store, nil)
				_, err = e.Syncer.Full(t.Context(), testEmail)
				require.NoError(err)
				require.NotEmpty(api.listTokens)
				assert.Equal("page_1", api.listTokens[0], "restart must retry the failed page")
				reopenAcquisitionStore(t, e)
				assertAcquisitionPresent(t, e, "good", "lost", "tail")
			})
		}
	}
}

func TestAcquisitionFailureDurabilityFullAllowsRecordedErrors(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	e := acquisitionDurabilityEnv(t)
	source := e.CreateSource(t)
	seedMessages(e, 3, 1000, "good", "lost", "tail")
	e.Mock.MessagePages = [][]string{{"good"}, {"lost"}, {"tail"}}
	e.Mock.GetMessageError["lost"] = errors.New("synthetic fetch failure")
	summary, err := e.Syncer.Full(t.Context(), testEmail)
	require.NoError(err)
	assert.Equal(int64(1), summary.Errors)
	assertAcquisitionPresent(t, e, "good", "tail")
	reopenAcquisitionStore(t, e)
	delete(e.Mock.GetMessageError, "lost")
	e.SetHistory(1000) // No History event for this old full-sync acquisition.
	_, err = e.Syncer.Incremental(t.Context(), gmailCoverageSource(t, e))
	require.NoError(err)
	assertAcquisitionPresent(t, e, "lost")
	items, err := e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
	require.NoError(err)
	assert.Empty(items)
}

func TestAcquisitionFailureDurabilityFailedStatusWrite(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	e := acquisitionDurabilityEnv(t)
	source := e.CreateSourceWithHistory(t, "1000")
	seedMessages(e, 1, 1001, "lost")
	e.SetHistory(1001, historyAdded("lost"))
	e.Mock.GetMessageError["lost"] = errors.New("synthetic fetch failure")
	allowRecord := acquisitionWriteFault(t, e, "sync_run_items", "INSERT", "NEW.status = 'error'")
	allowFailure := acquisitionWriteFault(t, e, "sync_runs", "UPDATE", "NEW.status = 'failed'")
	_, err := e.Syncer.Incremental(t.Context(), source)
	require.ErrorIs(err, errAcquisitionFailureRecord)
	run, err := e.Store.GetLatestSync(source.ID)
	require.NoError(err)
	assert.Equal(store.SyncStatusRunning, run.Status)
	assert.Equal("1000", gmailCoverageSource(t, e).SyncCursor.String)
	reopenAcquisitionStore(t, e)
	allowRecord()
	allowFailure()
	delete(e.Mock.GetMessageError, "lost")
	_, err = e.Syncer.Incremental(t.Context(), gmailCoverageSource(t, e))
	require.NoError(err, "next owner recovers the abandoned run")
	assertAcquisitionPresent(t, e, "lost")
}
