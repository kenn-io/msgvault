package store_test

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCardDAVSyncRunLifecycle(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()

	started, err := st.StartCardDAVSyncRunContext(ctx, store.CardDAVSyncRunStart{
		Trigger: store.CardDAVSyncTriggerManual,
		Full:    true,
	})
	require.NoError(err)
	assert.Equal(store.CardDAVSyncRunRunning, started.State)
	assert.Equal(store.CardDAVSyncTriggerManual, started.Trigger)
	assert.True(started.Full)
	assert.Nil(started.FinishedAt)

	_, err = st.StartCardDAVSyncRunContext(ctx, store.CardDAVSyncRunStart{
		Trigger: store.CardDAVSyncTriggerScheduled,
	})
	require.ErrorIs(err, store.ErrCardDAVSyncActive)

	status, err := st.CardDAVSyncStatusContext(ctx)
	require.NoError(err)
	require.NotNil(status.Active)
	assert.Equal(started.ID, status.Active.ID)

	finished, err := st.FinishCardDAVSyncRunContext(ctx, started.ID, store.CardDAVSyncRunFinish{
		State: store.CardDAVSyncRunSucceeded,
		Books: 3, Created: 4, Updated: 5, Removed: 6,
	})
	require.NoError(err)
	assert.Equal(int64(3), finished.Books)
	assert.Equal(int64(4), finished.Created)
	assert.Equal(int64(5), finished.Updated)
	assert.Equal(int64(6), finished.Removed)
	assert.NotNil(finished.FinishedAt)
	assert.True(finished.FinishedAt.After(finished.StartedAt) || finished.FinishedAt.Equal(finished.StartedAt))

	status, err = st.CardDAVSyncStatusContext(ctx)
	require.NoError(err)
	assert.Nil(status.Active)
	require.NotNil(status.Latest)
	assert.Equal(finished.ID, status.Latest.ID)
	require.NotNil(status.LatestSuccessful)
	assert.Equal(finished.ID, status.LatestSuccessful.ID)

	_, err = st.FinishCardDAVSyncRunContext(ctx, started.ID, store.CardDAVSyncRunFinish{
		State: store.CardDAVSyncRunSucceeded,
	})
	require.ErrorIs(err, store.ErrCardDAVSyncRunTransition)
}

func TestCardDAVSyncRunConcurrentActiveClaim(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			<-start
			_, err := st.StartCardDAVSyncRunContext(ctx, store.CardDAVSyncRunStart{Trigger: store.CardDAVSyncTriggerManual})
			results <- err
		})
	}
	close(start)
	workers.Wait()
	close(results)

	var successes, activeConflicts int
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, store.ErrCardDAVSyncActive):
			activeConflicts++
		default:
			require.NoError(err)
		}
	}
	assert.Equal(1, successes)
	assert.Equal(1, activeConflicts)
	var activeRows int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM carddav_sync_runs WHERE state = 'running'`).Scan(&activeRows))
	assert.Equal(1, activeRows)
}

func TestCardDAVSyncRunTerminalStatesRetainCounters(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := t.Context()

	for _, tc := range []struct {
		name  string
		state store.CardDAVSyncRunState
	}{
		{name: "failed", state: store.CardDAVSyncRunFailed},
		{name: "cancelled", state: store.CardDAVSyncRunCancelled},
		{name: "partial", state: store.CardDAVSyncRunPartial},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			run, err := st.StartCardDAVSyncRunContext(ctx, store.CardDAVSyncRunStart{Trigger: store.CardDAVSyncTriggerScheduled})
			require.NoError(err)
			finished, err := st.FinishCardDAVSyncRunContext(ctx, run.ID, store.CardDAVSyncRunFinish{
				State: tc.state, Books: 7, Created: 8, Updated: 9, Removed: 10,
				ErrorCode: "remote_failure", ErrorMessage: "Safe public failure",
			})
			require.NoError(err)
			assert.Equal(tc.state, finished.State)
			assert.Equal(int64(7), finished.Books)
			assert.Equal(int64(8), finished.Created)
			assert.Equal(int64(9), finished.Updated)
			assert.Equal(int64(10), finished.Removed)
			assert.Equal("remote_failure", finished.ErrorCode)
		})
	}
}

func TestCardDAVSyncRunRejectsInvalidInputAndTransitions(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()

	_, err := st.StartCardDAVSyncRunContext(ctx, store.CardDAVSyncRunStart{Trigger: "startup"})
	require.ErrorIs(err, store.ErrCardDAVSyncRunInvalid)
	run, err := st.StartCardDAVSyncRunContext(ctx, store.CardDAVSyncRunStart{Trigger: store.CardDAVSyncTriggerManual})
	require.NoError(err)

	invalid := []store.CardDAVSyncRunFinish{
		{State: store.CardDAVSyncRunRunning},
		{State: store.CardDAVSyncRunFailed, Books: -1, ErrorCode: "remote_failure"},
		{State: store.CardDAVSyncRunFailed, ErrorCode: "Bad-Code"},
		{State: store.CardDAVSyncRunFailed},
		{State: store.CardDAVSyncRunSucceeded, ErrorCode: "remote_failure"},
	}
	for _, finish := range invalid {
		_, finishErr := st.FinishCardDAVSyncRunContext(ctx, run.ID, finish)
		require.ErrorIs(finishErr, store.ErrCardDAVSyncRunInvalid)
	}
	_, err = st.FinishCardDAVSyncRunContext(ctx, run.ID+99, store.CardDAVSyncRunFinish{
		State: store.CardDAVSyncRunFailed, ErrorCode: "remote_failure",
	})
	require.ErrorIs(err, store.ErrCardDAVSyncRunNotFound)

	for _, input := range []struct {
		limit  int
		before *int64
	}{
		{limit: -1}, {limit: 101}, {limit: 1, before: new(int64(0))}, {limit: 1, before: new(int64(-1))},
	} {
		_, listErr := st.ListCardDAVSyncRunsContext(ctx, input.limit, input.before)
		require.ErrorIs(listErr, store.ErrCardDAVSyncRunInvalid)
	}
}

func TestCardDAVSyncRunPaginationAndRetention(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	ids := make([]int64, 0, 103)
	for range 103 {
		run, err := st.StartCardDAVSyncRunContext(ctx, store.CardDAVSyncRunStart{Trigger: store.CardDAVSyncTriggerManual})
		require.NoError(err)
		ids = append(ids, run.ID)
		_, err = st.FinishCardDAVSyncRunContext(ctx, run.ID, store.CardDAVSyncRunFinish{State: store.CardDAVSyncRunSucceeded})
		require.NoError(err)
	}
	active, err := st.StartCardDAVSyncRunContext(ctx, store.CardDAVSyncRunStart{Trigger: store.CardDAVSyncTriggerScheduled})
	require.NoError(err)

	var terminalCount, activeCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM carddav_sync_runs WHERE state <> 'running'`)).Scan(&terminalCount))
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM carddav_sync_runs WHERE state = 'running'`)).Scan(&activeCount))
	assert.Equal(100, terminalCount)
	assert.Equal(1, activeCount)

	page1, err := st.ListCardDAVSyncRunsContext(ctx, 2, nil)
	require.NoError(err)
	require.Len(page1, 2)
	assert.Equal(active.ID, page1[0].ID)
	before := page1[1].ID
	page2, err := st.ListCardDAVSyncRunsContext(ctx, 2, &before)
	require.NoError(err)
	require.Len(page2, 2)
	assert.Less(page2[0].ID, before)
	assert.NotEqual(page1[1].ID, page2[0].ID)
	assert.Equal(ids[len(ids)-1], page1[1].ID)

	wantIDs := []int64{active.ID}
	for i := len(ids) - 1; i >= 3; i-- {
		wantIDs = append(wantIDs, ids[i])
	}
	gotIDs := make([]int64, 0, len(wantIDs))
	var cursor *int64
	for {
		page, pageErr := st.ListCardDAVSyncRunsContext(ctx, 17, cursor)
		require.NoError(pageErr)
		if len(page) == 0 {
			break
		}
		for _, run := range page {
			gotIDs = append(gotIDs, run.ID)
		}
		next := page[len(page)-1].ID
		cursor = &next
	}
	assert.Equal(wantIDs, gotIDs, "exclusive pagination must neither duplicate nor omit retained rows")
}

func TestCardDAVSyncRunRecoveryAndSafePublicErrors(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()

	orphan, err := st.StartCardDAVSyncRunContext(ctx, store.CardDAVSyncRunStart{Trigger: store.CardDAVSyncTriggerManual})
	require.NoError(err)
	recovered, err := st.RecoverCardDAVSyncRunsContext(ctx)
	require.NoError(err)
	assert.Equal(int64(1), recovered)
	runs, err := st.ListCardDAVSyncRunsContext(ctx, 0, nil)
	require.NoError(err)
	require.NotEmpty(runs)
	assert.Equal(orphan.ID, runs[0].ID)
	assert.Equal(store.CardDAVSyncRunFailed, runs[0].State)
	assert.Equal("daemon_restarted", runs[0].ErrorCode)
	assert.NotEmpty(runs[0].ErrorMessage)
	_, err = st.StartCardDAVSyncRunContext(ctx, store.CardDAVSyncRunStart{Trigger: store.CardDAVSyncTriggerScheduled})
	require.NoError(err)

	_, err = st.FinishCardDAVSyncRunContext(ctx, runs[0].ID, store.CardDAVSyncRunFinish{
		State: store.CardDAVSyncRunFailed, ErrorCode: "remote_failure",
	})
	require.ErrorIs(err, store.ErrCardDAVSyncRunTransition)
}

func TestCardDAVSyncRunSchemaIndexesAndSQLiteReopenRecovery(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	var indexCount int
	if st.IsPostgreSQL() {
		require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM pg_indexes
			WHERE schemaname = current_schema()
			  AND indexname IN ('idx_carddav_sync_runs_one_active', 'idx_carddav_sync_runs_state_id')`).Scan(&indexCount))
	} else {
		require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_master
			WHERE type = 'index'
			  AND name IN ('idx_carddav_sync_runs_one_active', 'idx_carddav_sync_runs_state_id')`).Scan(&indexCount))
	}
	assert.Equal(2, indexCount)
	_, err := st.DB().Exec(st.Rebind(`INSERT INTO carddav_sync_runs
		(trigger, full_sync, state, finished_at, error_code, error_message)
		VALUES (?, ?, 'failed', CURRENT_TIMESTAMP, 'remote_failure', ?)`),
		store.CardDAVSyncTriggerManual, false, strings.Repeat("x", 2001))
	require.Error(err, "schema must reject oversized public errors")
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO carddav_sync_runs
		(trigger, full_sync, state, finished_at, error_code, error_message)
		VALUES (?, ?, 'succeeded', CURRENT_TIMESTAMP, 'remote_failure', 'failure')`),
		store.CardDAVSyncTriggerManual, false)
	require.Error(err, "schema must reject errors on succeeded runs")

	if st.IsPostgreSQL() {
		return
	}
	path := filepath.Join(t.TempDir(), "reopen.db")
	first, err := store.OpenForTest(path)
	require.NoError(err)
	require.NoError(first.InitSchema())
	orphan, err := first.StartCardDAVSyncRunContext(t.Context(), store.CardDAVSyncRunStart{Trigger: store.CardDAVSyncTriggerManual})
	require.NoError(err)
	require.NoError(first.Close())

	reopened, err := store.OpenForTest(path)
	require.NoError(err)
	t.Cleanup(func() { _ = reopened.Close() })
	require.NoError(reopened.InitSchema())
	recovered, err := reopened.RecoverCardDAVSyncRunsContext(t.Context())
	require.NoError(err)
	assert.Equal(int64(1), recovered)
	runs, err := reopened.ListCardDAVSyncRunsContext(t.Context(), 1, nil)
	require.NoError(err)
	require.Len(runs, 1)
	assert.Equal(orphan.ID, runs[0].ID)
	assert.Equal(store.CardDAVSyncRunFailed, runs[0].State)
	_, err = reopened.StartCardDAVSyncRunContext(t.Context(), store.CardDAVSyncRunStart{Trigger: store.CardDAVSyncTriggerScheduled})
	require.NoError(err)
}

func TestCardDAVSyncRunSchemaErrorCodeConstraintParity(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	insert := st.Rebind(`INSERT INTO carddav_sync_runs
		(trigger, full_sync, state, finished_at, error_code, error_message)
		VALUES (?, ?, 'failed', CURRENT_TIMESTAMP, ?, 'Safe public failure')`)

	for _, code := range []string{"1bad", "_bad"} {
		_, err := st.DB().Exec(insert, store.CardDAVSyncTriggerManual, false, code)
		require.Error(err, "schema must reject error code %q", code)
	}

	validBoundary := "a" + strings.Repeat("_", 63)
	result, err := st.DB().Exec(insert, store.CardDAVSyncTriggerManual, false, validBoundary)
	require.NoError(err)
	rows, err := result.RowsAffected()
	require.NoError(err)
	assert.Equal(t, int64(1), rows)
}

func TestCardDAVSyncRunErrorProjectionIsUTF8BoundedAndRedacted(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()

	run, err := st.StartCardDAVSyncRunContext(ctx, store.CardDAVSyncRunStart{Trigger: store.CardDAVSyncTriggerManual})
	require.NoError(err)
	long := strings.Repeat("界", 1000)
	finished, err := st.FinishCardDAVSyncRunContext(ctx, run.ID, store.CardDAVSyncRunFinish{
		State: store.CardDAVSyncRunFailed, ErrorCode: "remote_failure", ErrorMessage: long,
	})
	require.NoError(err)
	assert.LessOrEqual(len(finished.ErrorMessage), 2000)
	assert.True(utf8.ValidString(finished.ErrorMessage))

	run, err = st.StartCardDAVSyncRunContext(ctx, store.CardDAVSyncRunStart{Trigger: store.CardDAVSyncTriggerManual})
	require.NoError(err)
	unsafe := "Authorization: Bearer private-token password=hunter2 BEGIN:VCARD https://contacts.example.test/book"
	finished, err = st.FinishCardDAVSyncRunContext(ctx, run.ID, store.CardDAVSyncRunFinish{
		State: store.CardDAVSyncRunFailed, ErrorCode: "remote_failure", ErrorMessage: unsafe,
	})
	require.NoError(err)
	assert.Equal("unsafe_error_redacted", finished.ErrorCode)
	assert.NotEqual(unsafe, finished.ErrorMessage)
	for _, marker := range []string{"authorization", "bearer", "hunter2", "begin:vcard", "https://"} {
		assert.NotContains(strings.ToLower(finished.ErrorMessage), marker)
	}

	var stored string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT error_message FROM carddav_sync_runs WHERE id = ?`), finished.ID).Scan(&stored))
	assert.Equal(finished.ErrorMessage, stored)

	run, err = st.StartCardDAVSyncRunContext(ctx, store.CardDAVSyncRunStart{Trigger: store.CardDAVSyncTriggerManual})
	require.NoError(err)
	finished, err = st.FinishCardDAVSyncRunContext(ctx, run.ID, store.CardDAVSyncRunFinish{
		State: store.CardDAVSyncRunFailed, ErrorCode: "password", ErrorMessage: "Safe-looking text",
	})
	require.NoError(err)
	assert.Equal("unsafe_error_redacted", finished.ErrorCode)
	assert.NotContains(strings.ToLower(finished.ErrorMessage), "password")

	run, err = st.StartCardDAVSyncRunContext(ctx, store.CardDAVSyncRunStart{Trigger: store.CardDAVSyncTriggerManual})
	require.NoError(err)
	finished, err = st.FinishCardDAVSyncRunContext(ctx, run.ID, store.CardDAVSyncRunFinish{
		State: store.CardDAVSyncRunFailed, ErrorCode: "remote_failure", ErrorMessage: string([]byte{'o', 'k', 0xff}),
	})
	require.NoError(err)
	assert.True(utf8.ValidString(finished.ErrorMessage))
	assert.Equal("ok�", finished.ErrorMessage)
}

func TestCardDAVSyncRunRedactsStandaloneCredentialMarkers(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := t.Context()

	for _, tc := range []struct {
		name    string
		message string
	}{
		{name: "api_key", message: "API_KEY=synthetic-value"},
		{name: "access_token", message: "access-token: synthetic-value"},
		{name: "refresh_token", message: "refreshToken=synthetic-value"},
		{name: "credential", message: "Credential: synthetic-value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			run, err := st.StartCardDAVSyncRunContext(ctx, store.CardDAVSyncRunStart{
				Trigger: store.CardDAVSyncTriggerManual,
			})
			require.NoError(err)
			finished, err := st.FinishCardDAVSyncRunContext(ctx, run.ID, store.CardDAVSyncRunFinish{
				State: store.CardDAVSyncRunFailed, ErrorCode: "remote_failure", ErrorMessage: tc.message,
			})
			require.NoError(err)
			assert.Equal("unsafe_error_redacted", finished.ErrorCode)
			assert.Equal("CardDAV sync failed; sensitive details were removed.", finished.ErrorMessage)

			var storedCode, storedMessage string
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT error_code, error_message
				FROM carddav_sync_runs WHERE id = ?`), finished.ID).Scan(&storedCode, &storedMessage))
			assert.Equal("unsafe_error_redacted", storedCode)
			assert.Equal("CardDAV sync failed; sensitive details were removed.", storedMessage)
		})
	}
}
