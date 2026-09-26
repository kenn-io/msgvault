package beeper

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

// budgetTestChat builds a single-network chat with n messages starting at base.
func budgetTestChat(id string, n int, base time.Time) *fakeChat {
	ch := &fakeChat{
		ID: id, AccountID: "signal", Network: "Signal", Title: id, Type: "single",
		Participants: []map[string]any{
			{"id": "@me:beeper.local", "fullName": "Test User", "isSelf": true},
			{"id": "@signal_ann:beeper.local", "fullName": "Ann"},
		},
	}
	for i := range n {
		ch.Msgs = append(ch.Msgs, fakeMsg{
			ID: id + "-" + strconv.Itoa(i), SortKey: i,
			Timestamp: base.Add(time.Duration(i) * time.Minute),
			Text:      "msg " + strconv.Itoa(i),
			SenderID:  "@signal_ann:beeper.local", SenderName: "Ann",
		})
	}
	ch.LastActivity = ch.Msgs[len(ch.Msgs)-1].Timestamp
	return ch
}

func stopAfterMessagePages(f *fakeBeeper, n int) func() bool {
	return func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.pagesServed >= n
	}
}

func countBeeperMessages(t *testing.T, imp *Importer) int {
	t.Helper()
	var n int
	require.NoError(t, imp.store.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='beeper'`).Scan(&n))
	return n
}

func TestImportStopsAtChatBoundaryAndHoldsWatermark(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	base := time.Now().Add(-30 * 24 * time.Hour).UTC().Truncate(time.Second)
	f := newFakeBeeper(t)
	f.addChat(budgetTestChat("!one:beeper.local", 5, base))
	f.addChat(budgetTestChat("!two:beeper.local", 5, base.Add(time.Hour)))
	f.addChat(budgetTestChat("!three:beeper.local", 5, base.Add(2*time.Hour)))
	imp, st, done := newTestImporter(t, f)
	defer done()

	// Stop after the first chat's backfill page, leaving later chats discoverable.
	sum, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal", ShouldStop: stopAfterMessagePages(f, 1)})
	require.NoError(err, "a stopped run completes normally")
	assert.True(sum.Stopped)
	assert.EqualValues(1, sum.ChatsProcessed)
	assert.Equal(5, countBeeperMessages(t, imp))

	src, err := st.GetOrCreateSource("beeper", "signal")
	require.NoError(err)
	run, err := st.GetLastSuccessfulSync(src.ID)
	require.NoError(err)
	assert.Equal("completed", run.Status)
	state, err := LoadSyncState(run.CursorAfter.String)
	require.NoError(err)
	assert.Empty(state.ListWatermark, "a stopped run must not advance discovery past unvisited chats")
	assert.Empty(state.LastTailScan)

	sum, err = imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)
	assert.False(sum.Stopped)
	assert.Equal(15, countBeeperMessages(t, imp), "the next run visits the chats the stopped run skipped")
}

func TestStoppedImportUpdatesTouchedConversationStats(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFakeBeeper(t)
	f.addChat(budgetTestChat("!stats:beeper.local", 50, time.Now().Add(-48*time.Hour)))
	imp, st, done := newTestImporter(t, f)
	defer done()
	sum, err := imp.Import(t.Context(), ImportOptions{
		AccountID: "signal", ShouldStop: stopAfterMessagePages(f, 1),
	})
	require.NoError(err)
	require.True(sum.Stopped)
	var count int
	var preview string
	require.NoError(st.DB().QueryRow(`SELECT message_count, COALESCE(last_message_preview, '') FROM conversations`).Scan(&count, &preview))
	assert.Equal(20, count, "the stopped run exposes its committed page in conversation lists")
	assert.Equal("msg 49", preview)
}

func TestImportStopDuringConversationStatsCompletesSync(t *testing.T) {
	testutil.SkipIfPostgres(t, "uses a SQLite trigger to cancel during conversation stats")
	for _, budgetExpiry := range []bool{false, true} {
		name := "yield"
		if budgetExpiry {
			name = "budget expiry"
		}
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newFakeBeeper(t)
			chat := budgetTestChat("!stats-yield:beeper.local", 2, time.Now().Add(-24*time.Hour))
			f.addChat(chat)
			imp, st, done := newTestImporter(t, f)
			defer done()
			st.DB().SetMaxOpenConns(1)

			_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
			require.NoError(err)
			source, err := st.GetOrCreateSource(sourceTypeBeeper, "signal")
			require.NoError(err)

			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			statsStarted := make(chan struct{})
			var startOnce sync.Once
			var stopAt time.Time
			conn, err := st.DB().Conn(context.Background())
			require.NoError(err)
			err = conn.Raw(func(driverConn any) error {
				sqliteConn, ok := driverConn.(*sqlite3.SQLiteConn)
				require.True(ok, "driver connection is SQLite")
				return sqliteConn.RegisterFunc("wait_for_beeper_stats_yield", func() (int, error) {
					startOnce.Do(func() { close(statsStarted) })
					if !stopAt.IsZero() {
						timer := time.NewTimer(time.Until(stopAt))
						defer timer.Stop()
						<-timer.C
						return 0, errors.New("conversation stats interrupted at scheduled budget boundary")
					}
					<-ctx.Done()
					return 0, nil
				}, true)
			})
			require.NoError(err)
			require.NoError(conn.Close())
			_, err = st.DB().Exec(`
				CREATE TRIGGER wait_during_beeper_stats
				BEFORE UPDATE OF message_count ON conversations
				WHEN NEW.message_count != OLD.message_count
				BEGIN
					SELECT wait_for_beeper_stats_yield();
				END
			`)
			require.NoError(err)
			f.appendMsg(chat.ID, fakeMsg{
				ID: "!stats-yield:beeper.local-2", SortKey: 2,
				Timestamp: time.Now().Add(-23 * time.Hour),
				Text:      "new message",
				SenderID:  "@signal_ann:beeper.local", SenderName: "Ann",
			})

			type importResult struct {
				sum *ImportSummary
				err error
			}
			resultCh := make(chan importResult, 1)
			options := ImportOptions{AccountID: "signal"}
			if budgetExpiry {
				stopAt = time.Now().Add(time.Second)
				options.StopAt = stopAt
			} else {
				options.ShouldStop = func() bool {
					return ctx.Err() != nil
				}
			}
			go func() {
				sum, importErr := imp.Import(ctx, options)
				resultCh <- importResult{sum: sum, err: importErr}
			}()
			select {
			case <-statsStarted:
			case <-time.After(10 * time.Second):
				require.FailNow("conversation stats update did not reach cancellation trigger")
			}
			if !budgetExpiry {
				cancel()
			}
			var result importResult
			select {
			case result = <-resultCh:
			case <-time.After(10 * time.Second):
				require.FailNow("stop during conversation stats did not stop the scheduled import")
			}
			require.NoError(result.err, "stopping during local maintenance completes the sync normally")
			require.NotNil(result.sum)
			assert.True(result.sum.Stopped)
			run, err := st.GetLastSuccessfulSync(source.ID)
			require.NoError(err)
			assert.Equal("completed", run.Status)
			_, err = st.DB().Exec(`DROP TRIGGER wait_during_beeper_stats`)
			require.NoError(err)

			_, err = imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
			require.NoError(err, "the next scheduled run resumes after the stats pass was deferred")
			assert.Equal(3, countBeeperMessages(t, imp))
		})
	}
}

func TestImportStopsBeforeStaleRederivationWhenBudgetIsSpent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFakeBeeper(t)
	imp, st, done := newTestImporter(t, f)
	defer done()

	sum, err := imp.Import(context.Background(), ImportOptions{
		AccountID: "signal",
		StopAt:    time.Now().Add(-time.Second),
	})
	require.NoError(err, "an exhausted scheduled budget yields normally")
	require.NotNil(sum)
	assert.True(sum.Stopped)

	src, err := st.GetOrCreateSource(sourceTypeBeeper, "signal")
	require.NoError(err)
	var runs int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM sync_runs WHERE source_id = ?`,
	), src.ID).Scan(&runs))
	assert.Zero(runs, "a stopped setup phase must not start a sync run")
	assert.Empty(f.requests(), "a stopped setup phase must not call Beeper")
}

func TestImportStopsAtBackfillPageBoundary(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	base := time.Now().Add(-30 * 24 * time.Hour).UTC().Truncate(time.Second)
	f := newFakeBeeper(t)
	f.addChat(budgetTestChat("!big:beeper.local", 60, base))
	imp, st, done := newTestImporter(t, f)
	defer done()

	sum, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal", ShouldStop: stopAfterMessagePages(f, 1)})
	require.NoError(err)
	assert.True(sum.Stopped)
	assert.Equal(f.pageSize, countBeeperMessages(t, imp), "stopped after one backfill page")

	src, err := st.GetOrCreateSource("beeper", "signal")
	require.NoError(err)
	run, err := st.GetLastSuccessfulSync(src.ID)
	require.NoError(err)
	state, err := LoadSyncState(run.CursorAfter.String)
	require.NoError(err)
	require.NotNil(state.Chats["!big:beeper.local"])
	assert.False(state.Chats["!big:beeper.local"].Done, "stopped backfill stays resumable")

	_, err = imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)
	assert.Equal(60, countBeeperMessages(t, imp))
}

func TestImportStopsAfterIncrementalPageAndResumesFromCursor(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	base := time.Now().Add(-30 * 24 * time.Hour).UTC().Truncate(time.Second)
	f := newFakeBeeper(t)
	chat := budgetTestChat("!incremental:beeper.local", 2, base)
	f.addChat(chat)
	imp, _, done := newTestImporter(t, f)
	defer done()

	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)
	for i := range 6 {
		f.appendMsg(chat.ID, fakeMsg{
			ID: chat.ID + "-new-" + strconv.Itoa(i), SortKey: i + 2,
			Timestamp: base.Add(time.Duration(i+2) * time.Minute),
			Text:      "new message " + strconv.Itoa(i),
			SenderID:  "@signal_ann:beeper.local", SenderName: "Ann",
		})
	}
	f.pageSize = 2
	f.resetRequests()
	f.mu.Lock()
	f.pagesServed = 0
	f.mu.Unlock()

	sum, err := imp.Import(context.Background(), ImportOptions{
		AccountID: "signal", ShouldStop: func() bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.pagesServed >= 1
		},
	})
	require.NoError(err, "cooperative stop completes the scheduled run")
	assert.True(sum.Stopped)
	assert.Equal(4, countBeeperMessages(t, imp), "one incremental page is persisted")

	state := latestBeeperState(t, imp)
	require.NotNil(state.Chats[chat.ID])
	assert.Equal("3", state.Chats[chat.ID].Newest, "the page cursor is checkpointed before yielding")
	assert.NotContains(strings.Join(f.requests(), "\n"), "direction=before",
		"a stopped incremental pass skips reconciliation")

	sum, err = imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)
	assert.False(sum.Stopped)
	assert.Equal(8, countBeeperMessages(t, imp), "the next run resumes after the checkpointed page")
}

func TestImportBudgetCancelsBlockedMessagePageAndResumes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	f := newFakeBeeper(t)
	chat := budgetTestChat("!deadline:beeper.local", 2, base)
	f.addChat(chat)
	imp, _, done := newTestImporter(t, f)
	defer done()
	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)
	f.appendMsg(chat.ID, fakeMsg{
		ID: chat.ID + "-new", SortKey: 2, Timestamp: base.Add(2 * time.Minute),
		Text: "new message", SenderID: "@signal_ann:beeper.local", SenderName: "Ann",
	})
	started := make(chan struct{})
	f.mu.Lock()
	f.blockMessageListChatID = chat.ID
	f.messageListStarted = started
	f.mu.Unlock()

	sumCh := make(chan *ImportSummary, 1)
	errCh := make(chan error, 1)
	go func() {
		sum, importErr := imp.Import(context.Background(), ImportOptions{
			AccountID: "signal", StopAt: time.Now().Add(150 * time.Millisecond),
		})
		sumCh <- sum
		errCh <- importErr
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		require.FailNow("scheduled import did not enter the blocked provider request")
	}
	select {
	case sum := <-sumCh:
		require.NoError(<-errCh, "a spent request budget completes as resumable work")
		require.NotNil(sum)
		assert.True(sum.Stopped)
	case <-time.After(2 * time.Second):
		require.FailNow("request context did not cancel at the scheduled budget")
	}
	assert.Equal(2, countBeeperMessages(t, imp), "the interrupted page must not advance its cursor")

	sum, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)
	assert.False(sum.Stopped)
	assert.Equal(3, countBeeperMessages(t, imp), "the next run resumes the interrupted page")
}

func TestImportStopsAfterReconcilePage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	f := newFakeBeeper(t)
	f.pageSize = 2
	chat := budgetTestChat("!reconcile:beeper.local", 8, base)
	f.addChat(chat)
	imp, _, done := newTestImporter(t, f)
	defer done()

	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)
	f.resetRequests()
	f.mu.Lock()
	f.pagesServed = 0
	f.mu.Unlock()

	sum, err := imp.Import(context.Background(), ImportOptions{
		AccountID: "signal", ShouldStop: func() bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.pagesServed >= 2
		},
	})
	require.NoError(err)
	assert.True(sum.Stopped, "reconciliation yields at its page boundary")
	messagePages := 0
	for _, req := range f.requests() {
		if strings.HasSuffix(req, "/messages") || strings.Contains(req, "/messages?") {
			messagePages++
		}
	}
	assert.Equal(2, messagePages, "one incremental head check and one reconciliation page are fetched")
}

func TestImportStopsChatEnumerationAtPageBoundary(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	base := time.Now().Add(-30 * 24 * time.Hour).UTC().Truncate(time.Second)
	f := newFakeBeeper(t)
	f.chatPageSize = 1
	for i := range 4 {
		f.addChat(budgetTestChat("!enumerate"+strconv.Itoa(i)+":beeper.local", 2, base.Add(time.Duration(i)*time.Hour)))
	}
	imp, _, done := newTestImporter(t, f)
	defer done()

	sum, err := imp.Import(context.Background(), ImportOptions{
		AccountID: "signal", ShouldStop: func() bool {
			for _, request := range f.requests() {
				if strings.HasPrefix(request, "/v1/chats/search?") {
					return true
				}
			}
			return false
		},
	})
	require.NoError(err, "stopping enumeration completes the scheduled run")
	assert.True(sum.Stopped)
	assert.Zero(sum.ChatsProcessed)
	assert.Zero(countBeeperMessages(t, imp))

	searchPages := 0
	for _, req := range f.requests() {
		if strings.HasPrefix(req, "/v1/chats/search?") {
			searchPages++
		}
	}
	assert.Equal(1, searchPages, "enumeration yields before fetching another page")
}

func TestImportResumesTailProbeAfterBudgetStop(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	f := newFakeBeeper(t)
	chat := budgetTestChat("!tailprobe:beeper.local", 6, base)
	chat.TailCountdown = true
	for i := range chat.Msgs {
		chat.Msgs[i].IsHidden = true
	}
	f.addChat(chat)
	imp, _, done := newTestImporter(t, f)
	defer done()
	oldInterval := tailScanInterval
	tailScanInterval = 0
	t.Cleanup(func() { tailScanInterval = oldInterval })

	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)
	state := latestBeeperState(t, imp)
	require.True(state.Chats[chat.ID].Done)

	f.mu.Lock()
	f.pagesServed = 0
	f.mu.Unlock()
	sum, err := imp.Import(context.Background(), ImportOptions{
		AccountID: "signal", ShouldStop: func() bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.pagesServed >= 1
		},
	})
	require.NoError(err)
	assert.True(sum.Stopped, "the tail probe yields after a message page")
	state = latestBeeperState(t, imp)
	require.NotNil(state.Chats[chat.ID])
	assert.NotEmpty(state.Chats[chat.ID].TailProbeCursor, "the next run resumes at the next probe page")

	_, err = imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)
	state = latestBeeperState(t, imp)
	assert.Empty(state.Chats[chat.ID].TailProbeCursor, "a completed probe clears its temporary cursor")
}

func latestBeeperState(t *testing.T, imp *Importer) *SyncState {
	t.Helper()
	src, err := imp.store.GetOrCreateSource("beeper", "signal")
	require.NoError(t, err)
	run, err := imp.store.GetLastSuccessfulSync(src.ID)
	require.NoError(t, err)
	state, err := LoadSyncState(run.CursorAfter.String)
	require.NoError(t, err)
	return state
}

func TestImportRunsTailsBeforeBackfill(t *testing.T) {
	require := require.New(t)
	now := time.Now().UTC().Truncate(time.Second)
	f := newFakeBeeper(t)
	f.chatPageSize = 10 // newest-activity-first listing, like the live API
	tail := budgetTestChat("!tailchat:beeper.local", 3, now.Add(-30*24*time.Hour))
	f.addChat(tail)
	imp, _, done := newTestImporter(t, f)
	defer done()
	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err, "first run finishes the tail chat's backfill")

	f.appendMsg(tail.ID, fakeMsg{
		ID: "!tailchat:beeper.local-new", SortKey: 100, Timestamp: now.Add(-3 * time.Hour),
		Text: "new tail message", SenderID: "@signal_ann:beeper.local", SenderName: "Ann",
	})
	f.addChat(budgetTestChat("!backlog:beeper.local", 30, now.Add(-2*time.Hour)))
	f.resetRequests()

	_, err = imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)

	tailAt, backlogAt := -1, -1
	for i, req := range f.requests() {
		if !strings.Contains(req, "/messages?") {
			continue
		}
		if tailAt < 0 && strings.Contains(req, "tailchat") {
			tailAt = i
		}
		if backlogAt < 0 && strings.Contains(req, "backlog") {
			backlogAt = i
		}
	}
	require.GreaterOrEqual(tailAt, 0, "tail chat was fetched")
	require.GreaterOrEqual(backlogAt, 0, "backlog chat was fetched")
	assert.Less(t, tailAt, backlogAt, "incremental tails run before history backfill")
}

func TestImportTailScanProgressesAcrossBudgetedRuns(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	base := time.Now().Add(-40 * 24 * time.Hour).UTC().Truncate(time.Second)
	f := newFakeBeeper(t)
	for i := range 4 {
		f.addChat(budgetTestChat("!quiet"+strconv.Itoa(i)+":beeper.local", 3, base.Add(time.Duration(i)*time.Hour)))
	}
	imp, st, done := newTestImporter(t, f)
	defer done()
	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err, "first run finishes every chat and a tail scan")

	oldInterval := tailScanInterval
	tailScanInterval = 0
	t.Cleanup(func() { tailScanInterval = oldInterval })
	src, err := st.GetOrCreateSource("beeper", "signal")
	require.NoError(err)
	lastTailScan := func() string {
		run, err := st.GetLastSuccessfulSync(src.ID)
		require.NoError(err)
		state, err := LoadSyncState(run.CursorAfter.String)
		require.NoError(err)
		return state.LastTailScan
	}
	before := lastTailScan()
	time.Sleep(1100 * time.Millisecond) // tail-scan stamps have second precision

	// Stop after three completed chats, then finish the remaining probe on the
	// next scheduled run.
	completedChats := 0
	budgetedRun := func() ImportOptions {
		completedChats = 0
		return ImportOptions{
			AccountID:  "signal",
			ShouldStop: func() bool { return completedChats >= 3 },
			Progress:   func(string) { completedChats++ },
		}
	}
	sum, err := imp.Import(context.Background(), budgetedRun())
	require.NoError(err)
	require.True(sum.Stopped)
	assert.Equal(before, lastTailScan(), "an unfinished tail scan is not recorded")

	sum, err = imp.Import(context.Background(), budgetedRun())
	require.NoError(err)
	assert.False(sum.Stopped, "the second run resumes the scan instead of restarting it")
	assert.NotEqual(before, lastTailScan(), "the scan completes across budgeted runs")
}

// Every tick gets the same small budget. Completed chats must not consume
// that budget again while later chats still need their first visit.
func TestImportBudgetedCycleReachesEveryChat(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFakeBeeper(t)
	base := time.Now().Add(-48 * time.Hour)
	for _, id := range []string{"!first:beeper.local", "!second:beeper.local", "!third:beeper.local"} {
		f.addChat(budgetTestChat(id, 2, base))
	}
	imp, _, done := newTestImporter(t, f)
	defer done()
	for tick := range 3 {
		completed := 0
		_, err := imp.Import(t.Context(), ImportOptions{
			AccountID:  "signal",
			ShouldStop: func() bool { return completed >= 1 },
			Progress:   func(string) { completed++ },
		})
		require.NoError(err)
		if tick == 0 {
			f.appendMsg("!first:beeper.local", fakeMsg{
				ID: "new-during-cycle", SortKey: 2, Timestamp: time.Now(),
				Text: "new activity", SenderID: "@signal_ann:beeper.local", SenderName: "Ann",
			})
		}
	}
	assert.Equal(6, countBeeperMessages(t, imp), "successive ticks reach every chat")
	assert.Equal(formatWatermark(base.Add(time.Minute)), latestBeeperState(t, imp).ListWatermark,
		"new activity on a skipped chat remains beyond the cycle watermark")
	_, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal"})
	require.NoError(err)
	assert.Equal(7, countBeeperMessages(t, imp), "the next cycle imports new activity")
}

func TestImportCleanupDeadlineAfterEarlyPreemption(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		opts := ImportOptions{StopAt: time.Now().Add(2 * time.Minute)}
		cleanup, stop := opts.finalizeContext(ctx)
		defer stop()
		require.NoError(t, cleanup.Err(), "preemption still allows checkpoint cleanup")
		time.Sleep(15 * time.Second)
		synctest.Wait()
		require.ErrorIs(t, cleanup.Err(), context.DeadlineExceeded, "cleanup gets at most fifteen seconds")
	})
}

func TestImportBudgetAtCompletedChatStillAdvancesCycle(t *testing.T) {
	f := newFakeBeeper(t)
	for _, id := range []string{"!one:beeper.local", "!two:beeper.local", "!three:beeper.local"} {
		f.addChat(budgetTestChat(id, 2, time.Now().Add(-48*time.Hour)))
	}
	imp, _, done := newTestImporter(t, f)
	defer done()
	for range 3 {
		f.mu.Lock()
		pages := f.pagesServed
		f.mu.Unlock()
		_, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", ShouldStop: stopAfterMessagePages(f, pages+2)})
		require.NoError(t, err)
	}
	assert.Equal(t, 6, countBeeperMessages(t, imp), "a completed chat stays visited when cleanup notices the stop")
}
