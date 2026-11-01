package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
)

// Fail at observable provider boundaries, after the preceding durable write.
type recoveryStateAPI struct {
	*gmail.MockAPI

	fault                       string
	crash                       bool
	lists, snapshots, histories int
}

func (a *recoveryStateAPI) interrupt() error {
	if a.crash {
		os.Exit(42)
	} // Deliberately bypass sync/store defers in a child process.
	return errors.New("synthetic interruption")
}

func (a *recoveryStateAPI) GetProfile(ctx context.Context) (*gmail.Profile, error) {
	if a.fault == "profile" {
		return nil, a.interrupt()
	}
	return a.MockAPI.GetProfile(ctx)
}

func (a *recoveryStateAPI) ListMessages(ctx context.Context, query, token string) (*gmail.MessageListResponse, error) {
	a.lists++
	if a.fault == "enumerate" && a.lists == 2 {
		return nil, a.interrupt()
	}
	return a.MockAPI.ListMessages(ctx, query, token)
}

func (a *recoveryStateAPI) ListCompleteMessageSnapshot(ctx context.Context, token string) (*gmail.MessageListResponse, error) {
	a.snapshots++
	if a.fault == "snapshot" && a.snapshots == 2 {
		return nil, a.interrupt()
	}
	return a.MockAPI.ListCompleteMessageSnapshot(ctx, token)
}

func (a *recoveryStateAPI) ListHistory(ctx context.Context, start uint64, token string) (*gmail.HistoryResponse, error) {
	a.histories++
	if a.fault == "history-page" && token == "" {
		r, err := a.MockAPI.ListHistory(ctx, start, token)
		if err == nil {
			r.NextPageToken = "history-second"
		}
		return r, err
	}
	if a.fault == "catchup" || a.fault == "history-page" {
		_, _ = a.MockAPI.ListHistory(ctx, start, token)
		return nil, a.interrupt()
	}
	return a.MockAPI.ListHistory(ctx, start, token)
}

func TestGmailRecoveryCheckpointWrites(t *testing.T) {
	for _, phase := range []string{"inherit", "page", "reconcile", "catchup"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			require := require.New(t)
			assert := assert.New(t)

			e := recoveryStateEnv(t, true)
			condition := "NEW.cursor_after = '15000'"
			switch phase {
			case "page":
				condition = "NEW.cursor_before LIKE '%page_2%'"
			case "reconcile":
				condition = `NEW.cursor_before LIKE '%"phase":"reconcile"%'`
			case "catchup":
				condition = `NEW.cursor_before LIKE '%"phase":"catchup"%'`
			}
			_, err := e.Store.DB().Exec(`CREATE TRIGGER recovery_checkpoint_fault BEFORE UPDATE OF cursor_before ON sync_runs WHEN ` + condition + ` BEGIN SELECT RAISE(ABORT,'synthetic checkpoint write failure'); END`)
			require.NoError(err)
			for range 2 {
				_, err = e.Syncer.Full(t.Context(), testEmail)
				require.ErrorContains(err, "synthetic checkpoint write failure")
				assert.Equal("1000", gmailCoverageSource(t, e).SyncCursor.String)
				assert.Empty(e.Mock.HistoryCalls, "no catch-up after a rejected phase write")
				gmailCoverageReopen(t, e)
			}
			_, err = e.Store.DB().Exec("DROP TRIGGER recovery_checkpoint_fault")
			require.NoError(err)
			_, err = e.Syncer.Full(t.Context(), testEmail)
			require.NoError(err)
			assert.Equal([]uint64{15000}, e.Mock.HistoryCalls)
			assertDeletedFromSource(t, e.Store, "gone", true)
		})
	}
}

func TestGmailRecoveryRejectsFilteredFullWhilePending(t *testing.T) {
	for _, option := range []string{"query", "limit"} {
		t.Run(option, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)

			e := recoveryStateEnv(t, true)
			e.Mock.HistoryError = errors.New("synthetic catch-up failure")
			_, err := e.Syncer.Full(t.Context(), testEmail)
			require.Error(err)
			gmailCoverageReopen(t, e)
			opts := DefaultOptions()
			if option == "query" {
				opts.Query = "in:inbox"
			} else {
				opts.Limit = 1
			}
			_, err = New(e.Mock, e.Store, opts).Full(t.Context(), testEmail)
			require.ErrorContains(err, "unfiltered, unlimited")
			assert.Equal("1000", gmailCoverageSource(t, e).SyncCursor.String)
			e.Mock.HistoryError = nil
			lists, snapshots := e.Mock.ListMessagesCalls, e.Mock.SnapshotListCalls
			// The direct Incremental entry must honor the pending recovery too.
			_, err = New(e.Mock, e.Store, nil).Incremental(t.Context(), gmailCoverageSource(t, e))
			require.NoError(err)
			assert.Equal(lists, e.Mock.ListMessagesCalls)
			assert.Equal(snapshots, e.Mock.SnapshotListCalls)
			for _, h := range e.Mock.HistoryCalls {
				assert.Equal(uint64(15000), h)
			}
		})
	}
}

func TestGmailRecoveryItemErrorsDoNotBlockCompletion(t *testing.T) {
	for _, phase := range []string{"acquire", "fetch", "ingest", "delete", "label-add", "label-remove"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			require := require.New(t)
			assert := assert.New(t)
			e := recoveryStateEnv(t, true)
			switch phase {
			case "acquire":
				e.Mock.MessagePages[0] = append(e.Mock.MessagePages[0], "new-acquisition")
				e.Mock.GetMessageError["new-acquisition"] = errors.New("synthetic acquisition failure")
			case "fetch":
				e.Mock.GetMessageError["history-only"] = errors.New("synthetic fetch failure")
			case "ingest":
				_, err := e.Store.DB().Exec(`CREATE TRIGGER item_fault BEFORE INSERT ON messages WHEN NEW.source_message_id='history-only' BEGIN SELECT RAISE(ABORT,'synthetic ingest failure'); END`)
				require.NoError(err)
			case "delete":
				e.Mock.HistoryRecords = []gmail.HistoryRecord{historyDeleted("m0")}
				_, err := e.Store.DB().Exec(`CREATE TRIGGER item_fault BEFORE UPDATE OF deleted_from_source_at ON messages WHEN OLD.source_message_id='m0' BEGIN SELECT RAISE(ABORT,'synthetic delete failure'); END`)
				require.NoError(err)
			case "label-add":
				e.Mock.HistoryRecords = []gmail.HistoryRecord{historyLabelAdded("m0", "STARRED")}
				_, err := e.Store.DB().Exec(`CREATE TRIGGER item_fault BEFORE INSERT ON message_labels BEGIN SELECT RAISE(ABORT,'synthetic label failure'); END`)
				require.NoError(err)
			case "label-remove":
				e.Mock.HistoryRecords = []gmail.HistoryRecord{historyLabelRemoved("m0", "INBOX")}
				_, err := e.Store.DB().Exec(`CREATE TRIGGER item_fault BEFORE DELETE ON message_labels BEGIN SELECT RAISE(ABORT,'synthetic label failure'); END`)
				require.NoError(err)
			}
			// A message that keeps failing must not hold the source in recovery.
			summary, err := e.Syncer.Full(t.Context(), testEmail)
			require.NoError(err)
			assert.Equal("20000", gmailCoverageSource(t, e).SyncCursor.String)
			assertDeletedFromSource(t, e.Store, "gone", true)
			if phase == "acquire" || phase == "fetch" || phase == "ingest" || phase == "delete" {
				assert.Positive(summary.Errors)
			}
			_, err = e.Store.DB().Exec("DROP TRIGGER IF EXISTS item_fault")
			require.NoError(err)
			if phase != "fetch" {
				return
			}
			// Catch-up fetch failures carry forward to the next incremental run.
			e.Mock.GetMessageError = nil
			_, err = e.Syncer.Incremental(t.Context(), gmailCoverageSource(t, e))
			require.NoError(err)
			assertRawDataExists(t, e.Store, "history-only")
		})
	}
}

func TestGmailRecoveryCrashWorker(t *testing.T) {
	path := os.Getenv("MSGVAULT_TEST_RECOVERY_CRASH_DB")
	if path == "" {
		return
	}
	st, err := store.OpenForTest(path)
	require.NoError(t, err)
	mock := gmail.NewMockAPI()
	mock.Profile = &gmail.Profile{EmailAddress: testEmail, HistoryID: 20000}
	mock.HistoryID = 20000
	for i := range 8 {
		id := fmt.Sprintf("m%d", i)
		mock.AddMessage(id, testMIME(), []string{"INBOX"})
		mock.MessagePages = append(mock.MessagePages, []string{id})
	}
	api := &recoveryStateAPI{MockAPI: mock, fault: os.Getenv("MSGVAULT_TEST_RECOVERY_CRASH_PHASE"), crash: true}
	_, err = New(api, st, nil).Full(t.Context(), testEmail)
	require.NoError(t, err, "worker must exit at the configured boundary")
}

func TestGmailRecoveryNoResumeAndExpiredHandoff(t *testing.T) {
	for _, mode := range []string{"no-resume", "expired-handoff"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)

			e := recoveryStateEnv(t, true)
			opts := DefaultOptions()
			if mode == "no-resume" {
				opts.NoResume = true
			} else {
				e.Mock.HistoryError = &gmail.NotFoundError{Path: "/history"}
			}
			_, err := New(e.Mock, e.Store, opts).Full(t.Context(), testEmail)
			require.NoError(t, err, "fresh profile cursor equals new H, so the second catch-up is a no-op")
			if mode == "expired-handoff" {
				assert.Equal([]uint64{15000}, e.Mock.HistoryCalls)
			} else {
				assert.Empty(e.Mock.HistoryCalls)
			}
			assert.Equal("20000", gmailCoverageSource(t, e).SyncCursor.String)
			assertDeletedFromSource(t, e.Store, "gone", true)
		})
	}
}

func TestGmailRecoveryRepeatedHandoffExpirationIsBounded(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	e := recoveryStateEnv(t, true)
	e.Mock.HistoryError = &gmail.NotFoundError{Path: "/history"}
	api := &recoveryProfileSequenceAPI{MockAPI: e.Mock, historyIDs: []uint64{20000, 21000, 22000, 23000}}
	_, err := New(api, e.Store, nil).Full(t.Context(), testEmail)
	require.ErrorIs(err, ErrHistoryExpired)
	assert.Equal([]uint64{15000, 22000}, e.Mock.HistoryCalls)
	assert.Equal("1000", gmailCoverageSource(t, e).SyncCursor.String)
	prior, err := e.Store.GetLatestCheckpointedSync(gmailCoverageSource(t, e).ID)
	require.NoError(err)
	assert.Equal("22000", prior.CursorAfter.String)
	gmailCoverageReopen(t, e)
	e.Mock.HistoryError = nil
	e.Mock.Profile.HistoryID = 25000
	lists, snapshots := e.Mock.ListMessagesCalls, e.Mock.SnapshotListCalls
	_, err = e.Syncer.Full(t.Context(), testEmail)
	require.NoError(err)
	assert.Equal(lists, e.Mock.ListMessagesCalls)
	assert.Equal(snapshots, e.Mock.SnapshotListCalls)
	assert.Equal(uint64(22000), e.Mock.HistoryCalls[len(e.Mock.HistoryCalls)-1])
}

func TestGmailRecoveryExpirationAndInitialPin(t *testing.T) {
	for _, interruptions := range []int{0, 1, 2, 4} {
		t.Run(strconv.Itoa(interruptions), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)

			e := gmailCoverageEnv(t)
			source := e.CreateSourceWithHistory(t, "1000")
			e.Mock.Profile.HistoryID = 15000
			e.Mock.HistoryError = &gmail.NotFoundError{Path: "/history"}
			_, err := e.Store.DB().Exec(`CREATE TRIGGER pin_fault BEFORE UPDATE OF cursor_after ON sync_runs WHEN NEW.cursor_after='15000' BEGIN SELECT RAISE(ABORT,'synthetic pin failure'); END`)
			require.NoError(err)
			for range interruptions {
				_, err = e.Syncer.IncrementalWithHistoryRecovery(t.Context(), source, nil)
				require.ErrorContains(err, "synthetic pin failure")
				assert.Equal("1000", gmailCoverageSource(t, e).SyncCursor.String)
				assert.Zero(e.Mock.ListMessagesCalls, "no enumeration before durable H")
				gmailCoverageReopen(t, e)
			}
			_, err = e.Store.DB().Exec("DROP TRIGGER pin_fault")
			require.NoError(err)
			_, err = e.Syncer.IncrementalWithHistoryRecovery(t.Context(), source, nil)
			require.NoError(err)
			assert.Equal("15000", gmailCoverageSource(t, e).SyncCursor.String)
			for _, h := range e.Mock.HistoryCalls {
				assert.Equal(uint64(1000), h, "only expiration detection queries the old cursor; H is already current")
			}
		})
	}
}

func TestGmailRecoveryLegacyTwoInterruptionsThenFull(t *testing.T) {
	e := recoveryStateEnv(t, true) // First interruption: historical narrower recovery.
	e.Syncer = New(&recoveryStateAPI{MockAPI: e.Mock, fault: "enumerate"}, e.Store, nil)
	_, err := e.Syncer.Full(t.Context(), testEmail) // Second interruption: new page saved.
	require.Error(t, err)
	gmailCoverageReopen(t, e)
	e.Syncer = New(e.Mock, e.Store, nil)
	_, err = e.Syncer.Full(t.Context(), testEmail)
	require.NoError(t, err)
	assert.Equal(t, []uint64{15000}, e.Mock.HistoryCalls)
	assertDeletedFromSource(t, e.Store, "gone", true)
	assertRawDataExists(t, e.Store, "history-only")
}

type recoveryProgress struct{ complete []*gmail.SyncSummary }

func (*recoveryProgress) OnStart(int64)                  {}
func (*recoveryProgress) OnProgress(int64, int64, int64) {}
func (p *recoveryProgress) OnComplete(summary *gmail.SyncSummary) {
	p.complete = append(p.complete, summary)
}
func (*recoveryProgress) OnError(error) {}

func TestGmailRecoveryCompletionAndPublicationRange(t *testing.T) {
	for _, noop := range []bool{false, true} {
		t.Run(strconv.FormatBool(noop), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)

			e := recoveryStateEnv(t, true)
			if noop {
				e.Mock.Profile.HistoryID = 15000
			}
			source := gmailCoverageSource(t, e)
			var participant int64
			require.NoError(e.Store.DB().QueryRow("SELECT sender_id FROM messages WHERE source_id=? AND source_message_id='gone'", source.ID).Scan(&participant))
			person, _, err := e.Store.CreatePersonFromParticipant(participant)
			require.NoError(err)
			_, err = e.Store.SetPersonTrackingContext(t.Context(), person.ID, true)
			require.NoError(err)
			var lower int64
			require.NoError(e.Store.DB().QueryRow("SELECT lower_sequence FROM person_sweep_sync_publications WHERE sync_run_id=(SELECT MAX(id) FROM sync_runs)").Scan(&lower))
			progress := &recoveryProgress{}
			syncer := New(e.Mock, e.Store, nil)
			syncer.WithProgress(progress)
			hooks := 0
			syncer.WithSuccessfulSyncHook("synthetic", func(ctx context.Context, src *store.Source, changed bool) error {
				hooks++
				assert.True(changed)
				assert.Equal(gmailCoverageSource(t, e).SyncCursor.String, strconv.FormatUint(e.Mock.Profile.HistoryID, 10))
				return nil
			})
			summary, err := syncer.Full(t.Context(), testEmail)
			require.NoError(err)
			require.Len(progress.complete, 1)
			assert.Same(summary, progress.complete[0])
			var status string
			var inherited int64
			require.NoError(e.Store.DB().QueryRow("SELECT status FROM sync_runs WHERE id=?", summary.SyncRunID).Scan(&status))
			assert.Equal(store.SyncStatusCompleted, status, "summary points to the final committed run")
			require.NoError(e.Store.DB().QueryRow("SELECT lower_sequence FROM person_sweep_sync_publications WHERE sync_run_id=? AND source_id=?", summary.SyncRunID, source.ID).Scan(&inherited))
			assert.Equal(lower, inherited, "catch-up publishes the full recovery interval")
			assert.Equal(1, hooks)
			var latest, dirty int64
			require.NoError(e.Store.DB().QueryRow("SELECT MAX(sequence) FROM person_sweep_changes WHERE person_id=? AND source_id=?", person.ID, source.ID).Scan(&latest))
			require.NoError(e.Store.DB().QueryRow("SELECT dirty_through_sequence FROM person_sweep_work WHERE person_id=?", person.ID).Scan(&dirty))
			assert.GreaterOrEqual(dirty, latest, "full-phase changes are published without waiting for a gap detector")
		})
	}
}

func TestGmailRecoveryAbruptProcessExit(t *testing.T) {
	executable, err := os.Executable()
	require.NoError(t, err)
	for _, phase := range []string{"enumerate", "snapshot", "catchup", "history-page"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			require := require.New(t)
			assert := assert.New(t)

			e := recoveryStateEnv(t, true)
			for range 2 {
				cmd := exec.Command(executable, "-test.run=^TestGmailRecoveryCrashWorker$", "-test.timeout=60s")
				cmd.Env = []string{"MSGVAULT_TEST_RECOVERY_CRASH_DB=" + filepath.Join(e.TmpDir, "durable.db"), "MSGVAULT_TEST_RECOVERY_CRASH_PHASE=" + phase}
				output, err := cmd.CombinedOutput()
				var exit *exec.ExitError
				require.ErrorAs(err, &exit, string(output))
				require.Equal(42, exit.ExitCode(), string(output))
				gmailCoverageReopen(t, e)
				assert.Equal("1000", gmailCoverageSource(t, e).SyncCursor.String)
			}
			api := &recoveryStateAPI{MockAPI: e.Mock}
			e.Syncer = New(api, e.Store, nil)
			_, err := e.Syncer.Full(t.Context(), testEmail)
			require.NoError(err)
			require.Equal([]uint64{15000}, e.Mock.HistoryCalls)
			if phase == "catchup" || phase == "history-page" {
				assert.Zero(api.lists)
				assert.Zero(api.snapshots)
			}
			assertDeletedFromSource(t, e.Store, "gone", true)
		})
	}
}

func recoveryStateEnv(t *testing.T, legacy bool) *TestEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := store.OpenForTest(filepath.Join(dir, "durable.db"))
	require.NoError(t, err)
	require.NoError(t, st.InitSchema())
	mock := gmail.NewMockAPI()
	mock.Profile = &gmail.Profile{EmailAddress: testEmail, HistoryID: 1000}
	e := &TestEnv{Store: st, Mock: mock, Syncer: New(mock, st, nil), TmpDir: dir, Context: context.Background()}
	t.Cleanup(func() { require.NoError(t, e.Store.Close()) })
	seedMessages(e, 9, 1000, "gone", "m0", "m1", "m2", "m3", "m4", "m5", "m6", "m7")
	runFullSync(t, e)
	source := gmailCoverageSource(t, e)
	delete(e.Mock.Messages, "gone")
	e.Mock.MessagePages = nil
	for i := range 8 {
		e.Mock.MessagePages = append(e.Mock.MessagePages, []string{fmt.Sprintf("m%d", i)})
	}
	// The missed event exists only in History, outside acquisition and presence lists.
	e.Mock.AddMessage("history-only", testMIME(), []string{"INBOX"})
	e.Mock.HistoryRecords = []gmail.HistoryRecord{historyAdded("history-only")}
	e.Mock.Profile.HistoryID, e.Mock.HistoryID = 20000, 20000
	if legacy {
		// Releases before phase checkpoints saved a bare listing token.
		id, err := e.Store.StartSync(source.ID, "full")
		require.NoError(t, err)
		_, err = e.Store.DB().Exec("UPDATE sync_runs SET request_fingerprint=? WHERE id=?", store.GmailHistoryRecoveryRequestFingerprint, id)
		require.NoError(t, err)
		require.NoError(t, e.Store.ScopedToSync(source.ID, id).PinSyncHandoffCursorContext(t.Context(), id, "15000"))
		require.NoError(t, e.Store.UpdateSyncCheckpoint(id, &store.Checkpoint{PageToken: "page_1", MessagesProcessed: 1}))
		require.NoError(t, e.Store.FailSync(id, "synthetic legacy interruption"))
	} else {
		// Interrupt a real recovery after its first page, pinning H=15000.
		e.Mock.Profile.HistoryID = 15000
		api := &recoveryStateAPI{MockAPI: e.Mock, fault: "enumerate"}
		_, err := New(api, e.Store, nil).RecoverExpiredHistory(t.Context(), source)
		require.ErrorContains(t, err, "synthetic interruption")
		e.Mock.Profile.HistoryID = 20000
	}
	e.Mock.HistoryCalls = nil
	return e
}

func TestGmailRecoveryResumedCatchupKeepsAcquisitionCounts(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	e := recoveryStateEnv(t, false)
	e.Mock.AddMessage("new-ok", testMIME(), []string{"INBOX"})
	e.Mock.AddMessage("new-failing", testMIME(), []string{"INBOX"})
	e.Mock.MessagePages[1] = append(e.Mock.MessagePages[1], "new-ok", "new-failing")
	e.Mock.GetMessageError["new-failing"] = errors.New("synthetic acquisition failure")
	e.Mock.HistoryError = errors.New("synthetic catch-up failure")
	_, err := e.Syncer.Full(t.Context(), testEmail)
	require.ErrorContains(err, "synthetic catch-up failure")
	gmailCoverageReopen(t, e)

	// The retry skips enumeration; its summary still reports what enumeration did.
	e.Mock.HistoryError = nil
	lists := e.Mock.ListMessagesCalls
	summary, err := e.Syncer.Full(t.Context(), testEmail)
	require.NoError(err)
	assert.Equal(lists, e.Mock.ListMessagesCalls)
	assert.Equal(int64(2), summary.MessagesAdded, "new-ok from enumeration and history-only from catch-up")
	assert.Equal(int64(2), summary.Errors, "enumeration failure plus its failed acquisition retry during catch-up")
	assertRawDataExists(t, e.Store, "new-ok")
	assertRawDataExists(t, e.Store, "history-only")
}

func TestGmailRecoveryRepeatedInterruptions(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, phase := range []string{"profile", "enumerate", "snapshot", "reconcile", "catchup", "history-page", "publish"} {
			for _, interruptions := range []int{0, 1, 2, 4} {
				t.Run(fmt.Sprintf("legacy=%t/%s/interruptions=%d", legacy, phase, interruptions), func(t *testing.T) {
					t.Parallel()
					require := require.New(t)
					assert := assert.New(t)

					e := recoveryStateEnv(t, legacy)
					var listed, snapshots int
					for attempt := 0; attempt <= interruptions; attempt++ {
						gmailCoverageReopen(t, e)
						api := &recoveryStateAPI{MockAPI: e.Mock}
						if attempt < interruptions {
							api.fault = phase
						}
						e.Syncer = New(api, e.Store, nil)
						if attempt < interruptions && (phase == "reconcile" || phase == "publish") {
							query := `CREATE TRIGGER recovery_fault BEFORE UPDATE OF deleted_from_source_at ON messages BEGIN SELECT RAISE(ABORT, 'synthetic reconciliation interruption'); END`
							if phase == "publish" {
								query = `CREATE TRIGGER recovery_fault BEFORE UPDATE OF sync_cursor ON sources BEGIN SELECT RAISE(ABORT, 'synthetic publication interruption'); END`
							}
							_, err := e.Store.DB().Exec(query)
							require.NoError(err)
						}
						// Alternate public entry points across SQL close/reopen cycles.
						var summary *gmail.SyncSummary
						var err error
						if attempt%2 == 0 {
							summary, err = e.Syncer.Full(t.Context(), testEmail)
						} else {
							summary, err = e.Syncer.IncrementalWithHistoryRecovery(t.Context(), gmailCoverageSource(t, e), nil)
						}
						_, dropErr := e.Store.DB().Exec("DROP TRIGGER IF EXISTS recovery_fault")
						require.NoError(dropErr)
						if attempt < interruptions {
							require.Error(err)
							assert.Equal("1000", gmailCoverageSource(t, e).SyncCursor.String)
							if phase == "reconcile" {
								assertDeletedFromSource(t, e.Store, "gone", false)
							}
						} else {
							require.NoError(err)
							assert.Equal(uint64(20000), summary.FinalHistoryID)
							assert.Equal("20000", gmailCoverageSource(t, e).SyncCursor.String)
							assertDeletedFromSource(t, e.Store, "gone", true)
							assertRawDataExists(t, e.Store, "gone")
							assertRawDataExists(t, e.Store, "history-only")
						}
						if attempt > 0 && (phase == "catchup" || phase == "history-page" || phase == "publish") {
							assert.Equal(listed, e.Mock.ListMessagesCalls, "retry catch-up without acquisition")
							assert.Equal(snapshots, e.Mock.SnapshotListCalls, "retry catch-up without presence inventory")
						}
						listed, snapshots = e.Mock.ListMessagesCalls, e.Mock.SnapshotListCalls
					}
					{
						require.NotEmpty(e.Mock.HistoryCalls)
						for _, start := range e.Mock.HistoryCalls {
							assert.Equal(uint64(15000), start, "never repin an inherited handoff")
						}
					}
				})
			}
		}
	}
}
