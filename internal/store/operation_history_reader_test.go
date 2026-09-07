package store_test

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestOperationHistoryReaderWalksExactMergedOrder(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	instant := time.Date(2026, 8, 29, 2, 0, 0, 0, time.UTC)
	seedMergedOperationRuns(t, st, instant)
	assert.Equal(allOperationHistoryKinds, st.Kinds())

	want := []string{
		"carddav_sync:10", "carddav_sync:9",
		"person_sweep:run-b", "person_sweep:run-a",
		"source_sync:10", "source_sync:9",
	}
	var got []string
	var position *operations.Position
	for len(got) < len(want) {
		snapshot, err := st.ListRuns(t.Context(), operations.Query{Position: position, Limit: 1})
		require.NoError(err)
		require.NotEmpty(snapshot.Runs)
		assert.LessOrEqual(len(snapshot.Runs), 2, "reader returns at most limit+1")
		got = append(got, operationRunKey(t, snapshot.Runs[0]))
		if len(snapshot.Runs) > 1 {
			require.NotNil(snapshot.Position, "lookahead returns a snapshot-bound continuation")
			assert.Equal(snapshot.Runs[0].StartedAt, snapshot.Position.StartedAt)
			assert.Equal(snapshot.Runs[0].ID, snapshot.Position.ID,
				"continuation identifies the last row exposed at the requested limit")
			position = snapshot.Position
		} else {
			assert.Nil(snapshot.Position, "the terminal page has no continuation")
			position = &operations.Position{
				StartedAt: snapshot.Runs[0].StartedAt,
				ID:        snapshot.Runs[0].ID,
			}
		}
	}
	assert.Equal(want, got)
	after, err := st.ListRuns(t.Context(), operations.Query{Position: position, Limit: 1})
	require.NoError(err)
	assert.Empty(after.Runs)
	assert.Nil(after.Position)
}

func TestOperationHistoryReaderFiltersAndRejectsInvalidQueries(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	instant := time.Date(2026, 8, 29, 3, 0, 0, 0, time.UTC)
	seedMergedOperationRuns(t, st, instant)

	people, err := st.ListRuns(t.Context(), operations.Query{
		Kinds: []operations.Kind{operations.KindPersonSweep}, Limit: 100,
	})
	require.NoError(err)
	assert.Equal([]string{"person_sweep:run-b", "person_sweep:run-a"}, operationRunKeys(t, people.Runs))

	succeeded, err := st.ListRuns(t.Context(), operations.Query{
		States: []operations.State{operations.StateSucceeded}, Limit: 100,
	})
	require.NoError(err)
	require.Len(succeeded.Runs, 6)
	for _, run := range succeeded.Runs {
		assert.Equal(operations.StateSucceeded, run.State)
	}

	for _, query := range []operations.Query{{Limit: 0}, {Limit: 101}, {
		Kinds: []operations.Kind{operations.KindSourceSync, operations.KindCardDAVSync}, Limit: 10,
	}} {
		runs, queryErr := st.ListRuns(t.Context(), query)
		require.Error(queryErr)
		assert.Empty(runs.Runs)
	}
}

func TestOperationHistoryReaderOrdersNeighboringTimestamps(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	base := time.Date(2026, 8, 29, 3, 15, 0, 0, time.UTC)
	cardDAVID := insertCardDAVOperationRun(t, st, cardDAVOperationRunSeed{
		startedAt: base, trigger: "manual", state: "succeeded",
	})
	insertPersonSweepOperationRun(t, st, personSweepOperationSeed{
		id: "neighbor-person", trigger: "manual", state: "succeeded",
		startedAt: base.Add(time.Millisecond),
	})
	source := createOperationSource(t, st, "neighbor-operations@example.invalid")
	sourceID := insertSourceOperationRun(t, st, source.ID, sourceOperationRunSeed{
		startedAt: base.Add(time.Second), state: "running", processed: 1,
	})

	runs, err := st.ListRuns(t.Context(), operations.Query{Limit: 10})
	require.NoError(err)
	assert.Equal([]string{
		"source_sync:" + fmtInt64(sourceID),
		"person_sweep:neighbor-person",
		"carddav_sync:" + fmtInt64(cardDAVID),
	}, operationRunKeys(t, runs.Runs))

	running, err := st.ListRuns(t.Context(), operations.Query{
		States: []operations.State{operations.StateRunning}, Limit: 10,
	})
	require.NoError(err)
	assert.Equal([]string{"source_sync:" + fmtInt64(sourceID)}, operationRunKeys(t, running.Runs))
}

func TestOperationHistoryReaderPreservesMixedTimestampPrecision(t *testing.T) {
	st := testutil.NewTestStore(t)
	second := time.Date(2026, 8, 29, 3, 30, 0, 0, time.UTC)
	cardDAVID := insertCardDAVOperationRun(t, st, cardDAVOperationRunSeed{
		startedAt: second, trigger: "manual", state: "succeeded",
	})
	source := createOperationSource(t, st, "precision-operations@example.invalid")
	sourceID := insertSourceOperationRun(t, st, source.ID, sourceOperationRunSeed{
		startedAt: second, state: "completed", processed: 1,
	})
	personCursor, err := operations.NewTextID(operations.KindPersonSweep, "fractional-cursor")
	require.NoError(t, err)

	runs, err := st.ListRuns(t.Context(), operations.Query{
		Position: &operations.Position{StartedAt: second.Add(500 * time.Millisecond), ID: personCursor},
		Limit:    10,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{
		"carddav_sync:" + fmtInt64(cardDAVID),
		"source_sync:" + fmtInt64(sourceID),
	}, operationRunKeys(t, runs.Runs))
}

func TestOperationHistoryReaderDoesNotApplyIDTieAtFinerSourceCursor(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	second := time.Date(2026, 8, 29, 3, 45, 0, 0, time.UTC)
	source := createOperationSource(t, st, "source-fine-cursor@example.invalid")
	lowerID := insertSourceOperationRun(t, st, source.ID, sourceOperationRunSeed{
		startedAt: second, state: "completed", processed: 1,
	})
	higherID := insertSourceOperationRun(t, st, source.ID, sourceOperationRunSeed{
		startedAt: second, state: "completed", processed: 2,
	})
	cursorID := mustSourceOperationID(t, lowerID)

	runs, err := st.ListRuns(t.Context(), operations.Query{
		Kinds: []operations.Kind{operations.KindSourceSync},
		Position: &operations.Position{
			StartedAt: second.Add(500 * time.Millisecond), ID: cursorID,
		},
		Limit: 10,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{
		"source_sync:" + fmtInt64(higherID),
		"source_sync:" + fmtInt64(lowerID),
	}, operationRunKeys(t, runs.Runs))
}

func TestOperationHistoryReaderDoesNotApplyIDTieAtSubMillisecondPeopleCursor(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	stored := time.Date(2026, 8, 29, 3, 50, 0, 500_000_000, time.UTC)
	for _, id := range []string{"run-a", "run-b"} {
		insertPersonSweepOperationRun(t, st, personSweepOperationSeed{
			id: id, trigger: "manual", state: "succeeded", startedAt: stored,
		})
	}
	cursorID := mustPersonSweepOperationID(t, "run-a")
	runs, err := st.ListRuns(t.Context(), operations.Query{
		Kinds: []operations.Kind{operations.KindPersonSweep},
		Position: &operations.Position{
			StartedAt: stored.Add(500 * time.Microsecond), ID: cursorID,
		},
		Limit: 10,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"person_sweep:run-b", "person_sweep:run-a"}, operationRunKeys(t, runs.Runs))
}

func TestOperationHistoryReaderBoundsEveryAdapterToLimitPlusOne(t *testing.T) {
	st := testutil.NewTestStore(t)
	query := operations.Query{Limit: 1}
	for _, list := range []func(int) error{
		func(fetchLimit int) error {
			_, err := store.ListSourceOperationRunsWithFetchLimitForTest(
				st, t.Context(), query, fetchLimit)
			return err
		},
		func(fetchLimit int) error {
			_, err := store.ListPersonSweepOperationRunsWithFetchLimitForTest(
				st, t.Context(), query, fetchLimit)
			return err
		},
		func(fetchLimit int) error {
			_, err := store.ListCardDAVOperationRunsWithFetchLimitForTest(
				st, t.Context(), query, fetchLimit)
			return err
		},
		func(fetchLimit int) error {
			_, err := store.ListInvocationOperationRunsWithFetchLimitForTest(
				st, t.Context(), operations.KindMessageEmbedding, query, fetchLimit)
			return err
		},
		func(fetchLimit int) error {
			_, err := store.ListInvocationOperationRunsWithFetchLimitForTest(
				st, t.Context(), operations.KindPersonEmbedding, query, fetchLimit)
			return err
		},
		func(fetchLimit int) error {
			_, err := store.ListInvocationOperationRunsWithFetchLimitForTest(
				st, t.Context(), operations.KindDocumentExtraction, query, fetchLimit)
			return err
		},
		func(fetchLimit int) error {
			_, err := store.ListInvocationOperationRunsWithFetchLimitForTest(
				st, t.Context(), operations.KindDocumentEmbedding, query, fetchLimit)
			return err
		},
		func(fetchLimit int) error {
			_, err := store.ListInvocationOperationRunsWithFetchLimitForTest(
				st, t.Context(), operations.KindVisualEmbedding, query, fetchLimit)
			return err
		},
		func(fetchLimit int) error {
			_, err := store.ListPersonEnrichmentOperationRunsWithFetchLimitForTest(
				st, t.Context(), query, fetchLimit)
			return err
		},
	} {
		require.NoError(t, list(2))
		require.Error(t, list(3))
	}
}

func TestOperationHistoryReaderReturnsPartialRowsOnAdapterFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	instant := time.Date(2026, 8, 29, 4, 0, 0, 0, time.UTC)
	seedMergedOperationRuns(t, st, instant)
	_, err := st.DB().ExecContext(t.Context(),
		`ALTER TABLE person_sweep_runs RENAME TO person_sweep_runs_unavailable`)
	require.NoError(err)

	snapshot, err := st.ListRuns(t.Context(), operations.Query{Limit: 100})
	require.NoError(err)
	assert.Equal([]operations.Kind{operations.KindPersonSweep}, snapshot.UnavailableKinds)
	assert.Equal([]string{
		"carddav_sync:10", "carddav_sync:9", "source_sync:10", "source_sync:9",
	}, operationRunKeys(t, snapshot.Runs))

	cardDAVStatus, err := st.LaneStatus(t.Context(), operations.KindCardDAVSync)
	require.NoError(err)
	assert.Equal(operations.KindCardDAVSync, cardDAVStatus.Kind)
	_, err = st.LaneStatus(t.Context(), operations.KindPersonSweep)
	require.Error(err)
}

func TestOperationHistoryReaderGetDispatchesByTypedKind(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	instant := time.Date(2026, 8, 29, 5, 0, 0, 0, time.UTC)
	seedMergedOperationRuns(t, st, instant)

	runs, err := st.ListRuns(t.Context(), operations.Query{Limit: 100})
	require.NoError(err)
	for _, listed := range runs.Runs {
		detail, detailErr := st.GetRun(t.Context(), listed.ID)
		require.NoError(detailErr)
		assert.Equal(listed, detail)
	}

	cardDAVID, err := operations.NewInt64ID(operations.KindCardDAVSync, 9)
	require.NoError(err)
	sourceID, err := operations.NewInt64ID(operations.KindSourceSync, 9)
	require.NoError(err)
	cardDAV, err := st.GetRun(t.Context(), cardDAVID)
	require.NoError(err)
	source, err := st.GetRun(t.Context(), sourceID)
	require.NoError(err)
	assert.Equal(operations.KindCardDAVSync, cardDAV.ID.Kind())
	assert.Equal(operations.KindSourceSync, source.ID.Kind())
	assert.NotEqual(cardDAV, source)

	missing, err := operations.NewTextID(operations.KindPersonSweep, "missing")
	require.NoError(err)
	_, err = st.GetRun(t.Context(), missing)
	require.ErrorIs(err, store.ErrOperationRunNotFound)
}

func TestOperationHistoryReaderUsesOneCoherentSnapshot(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	before := time.Date(2026, 8, 29, 6, 0, 0, 250_000_000, time.UTC)
	after := before.Add(time.Hour)
	insertCardDAVOperationRun(t, st, cardDAVOperationRunSeed{
		startedAt: before, trigger: "manual", state: "succeeded",
	})
	insertPersonSweepOperationRun(t, st, personSweepOperationSeed{
		id: "snapshot-person", trigger: "manual", state: "succeeded", startedAt: before,
	})

	var once sync.Once
	store.SetOperationHistoryAfterAdapterReadHookForTest(st, func(kind operations.Kind) {
		if kind != operations.KindCardDAVSync {
			return
		}
		once.Do(func() {
			_, err := st.DB().ExecContext(t.Context(), st.Rebind(`
				UPDATE person_sweep_runs SET started_at = ?, completed_at = ? WHERE id = ?`),
				personSweepTimestampParam(st, after),
				personSweepTimestampParam(st, after.Add(time.Second)),
				"snapshot-person")
			require.NoError(err)
		})
	})
	t.Cleanup(func() { store.SetOperationHistoryAfterAdapterReadHookForTest(st, nil) })

	runs, err := st.ListRuns(t.Context(), operations.Query{Limit: 10})
	require.NoError(err)
	require.Len(runs.Runs, 2)
	for _, run := range runs.Runs {
		wantStartedAt := before
		if run.ID.Kind() == operations.KindCardDAVSync && !st.IsPostgreSQL() {
			wantStartedAt = before.Truncate(time.Second)
		}
		assert.Equal(wantStartedAt, run.StartedAt,
			"all adapters must observe the snapshot established by the first adapter")
	}

	store.SetOperationHistoryAfterAdapterReadHookForTest(st, nil)
	person, err := st.GetRun(t.Context(), mustPersonSweepOperationID(t, "snapshot-person"))
	require.NoError(err)
	assert.Equal(after, person.StartedAt,
		"the concurrent transition committed after the snapshot")
}

func TestOperationHistoryReaderLaneStatusUsesOneCoherentSnapshot(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	startedAt := time.Date(2026, 8, 29, 6, 30, 0, 0, time.UTC)
	source := createOperationSource(t, st, "status-snapshot@example.invalid")
	runID := insertSourceOperationRun(t, st, source.ID, sourceOperationRunSeed{
		startedAt: startedAt, state: "running", processed: 1,
	})

	var once sync.Once
	store.SetOperationHistoryStatusAfterActiveHookForTest(st, func(kind operations.Kind) {
		if kind != operations.KindSourceSync {
			return
		}
		once.Do(func() {
			_, err := st.DB().ExecContext(t.Context(), st.Rebind(`
				UPDATE sync_runs SET status = 'failed', completed_at = ? WHERE id = ?`),
				sourceOperationTimestampParam(st, startedAt.Add(time.Second)), runID)
			require.NoError(err)
		})
	})
	t.Cleanup(func() { store.SetOperationHistoryStatusAfterActiveHookForTest(st, nil) })

	status, err := st.LaneStatus(t.Context(), operations.KindSourceSync)
	require.NoError(err)
	require.NotNil(status.Active)
	require.NotNil(status.Latest)
	assert.Equal(operations.StateRunning, status.Active.State)
	assert.Equal(operations.StateRunning, status.Latest.State,
		"latest must share the active query's snapshot")
	assert.Nil(status.LatestSuccessful)

	store.SetOperationHistoryStatusAfterActiveHookForTest(st, nil)
	finished, err := st.GetRun(t.Context(), mustSourceOperationID(t, runID))
	require.NoError(err)
	assert.Equal(operations.StateFailed, finished.State,
		"the terminal transition committed after the status snapshot")
}

func seedMergedOperationRuns(t *testing.T, st *store.Store, instant time.Time) {
	t.Helper()
	source := createOperationSource(t, st, "merged-operations@example.invalid")
	for ordinal := 1; ordinal <= 10; ordinal++ {
		insertSourceOperationRun(t, st, source.ID, sourceOperationRunSeed{
			startedAt: instant, state: "completed", processed: int64(ordinal),
		})
		insertCardDAVOperationRun(t, st, cardDAVOperationRunSeed{
			startedAt: instant, trigger: "manual", state: "succeeded", books: int64(ordinal),
		})
	}
	_, err := st.DB().ExecContext(t.Context(), `DELETE FROM sync_runs WHERE id < 9`)
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), `DELETE FROM carddav_sync_runs WHERE id < 9`)
	require.NoError(t, err)
	for _, id := range []string{"run-a", "run-b"} {
		insertPersonSweepOperationRun(t, st, personSweepOperationSeed{
			id: id, trigger: "manual", state: "succeeded", startedAt: instant,
		})
	}
}

func operationRunKeys(t *testing.T, runs []operations.Run) []string {
	t.Helper()
	keys := make([]string, 0, len(runs))
	for _, run := range runs {
		keys = append(keys, operationRunKey(t, run))
	}
	return keys
}

func operationRunKey(t *testing.T, run operations.Run) string {
	t.Helper()
	if value, ok := run.ID.Int64(); ok {
		return string(run.ID.Kind()) + ":" + fmtInt64(value)
	}
	value, ok := run.ID.Text()
	require.True(t, ok)
	return string(run.ID.Kind()) + ":" + value
}

func fmtInt64(value int64) string {
	return strconv.FormatInt(value, 10)
}
