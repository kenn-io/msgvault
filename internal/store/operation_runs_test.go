package store_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestOperationRunsSourceProjectsStatesCountersAndFilters(t *testing.T) {
	newAssertions := assert.New
	newRequirements := require.New
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source := createOperationSource(t, st, "state-source@example.invalid")
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		seed      sourceOperationRunSeed
		wantState operations.State
		wantError *operations.PublicError
	}{
		{
			name:      "running",
			seed:      sourceOperationRunSeed{startedAt: base, state: "running", processed: 3},
			wantState: operations.StateRunning,
		},
		{
			name: "completed cleanly",
			seed: sourceOperationRunSeed{
				startedAt: base.Add(time.Minute), state: "completed", processed: 8,
				added: 2, updated: 3,
			},
			wantState: operations.StateSucceeded,
		},
		{
			name: "completed with item errors",
			seed: sourceOperationRunSeed{
				startedAt: base.Add(2 * time.Minute), state: "completed", processed: 8,
				added: 2, updated: 3, itemErrors: 1,
			},
			wantState: operations.StatePartial,
		},
		{
			name: "failed after adding",
			seed: sourceOperationRunSeed{
				startedAt: base.Add(3 * time.Minute), state: "failed", processed: 5, added: 1,
			},
			wantState: operations.StatePartial,
			wantError: sourceOperationPublicError(),
		},
		{
			name: "failed after updating",
			seed: sourceOperationRunSeed{
				startedAt: base.Add(4 * time.Minute), state: "failed", processed: 5, updated: 1,
			},
			wantState: operations.StatePartial,
			wantError: sourceOperationPublicError(),
		},
		{
			name: "failed after processed only",
			seed: sourceOperationRunSeed{
				startedAt: base.Add(5 * time.Minute), state: "failed", processed: 5,
			},
			wantState: operations.StateFailed,
			wantError: sourceOperationPublicError(),
		},
		{
			name: "exact legacy cancelled",
			seed: sourceOperationRunSeed{
				startedAt: base.Add(6 * time.Minute), state: "cancelled", processed: 1,
			},
			wantState: operations.StateCancelled,
		},
	}

	idsByState := make(map[operations.State][]int64)
	allIDs := make([]int64, 0, len(tests))
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := newRequirements(t)
			assert := newAssertions(t)
			id := insertSourceOperationRun(t, st, source.ID, test.seed)
			allIDs = append(allIDs, id)
			idsByState[test.wantState] = append(idsByState[test.wantState], id)

			got, err := store.GetSourceOperationRunForTest(
				st, t.Context(), mustSourceOperationID(t, id))
			require.NoError(err)
			assert.Equal(test.wantState, got.State)
			assert.Equal(test.wantError, got.Error)
			assert.Nil(got.Trigger)
			assert.Equal([]operations.PublicCounter{
				{Name: operations.CounterProcessed, Unit: operations.CounterUnitMessages, Value: test.seed.processed},
				{Name: operations.CounterAdded, Unit: operations.CounterUnitMessages, Value: test.seed.added},
				{Name: operations.CounterUpdated, Unit: operations.CounterUnitMessages, Value: test.seed.updated},
				{Name: operations.CounterItemErrors, Unit: operations.CounterUnitMessages, Value: test.seed.itemErrors},
			}, got.Counters)
			require.NoError(got.Validate())
		})
	}

	runs, err := store.ListSourceOperationRunsForTest(
		st, t.Context(), operations.Query{Limit: 100})
	require.NoError(err)
	require.Len(runs, len(allIDs))
	for index, run := range runs {
		assert.Equal(allIDs[len(allIDs)-1-index], sourceOperationIntID(t, run.ID))
	}

	for _, state := range []operations.State{
		operations.StateRunning,
		operations.StateSucceeded,
		operations.StatePartial,
		operations.StateFailed,
		operations.StateCancelled,
	} {
		filtered, filterErr := store.ListSourceOperationRunsForTest(st, t.Context(), operations.Query{
			States: []operations.State{state}, Limit: 100,
		})
		require.NoError(filterErr, state)
		gotIDs := make([]int64, 0, len(filtered))
		for _, run := range filtered {
			gotIDs = append(gotIDs, sourceOperationIntID(t, run.ID))
			assert.Equal(state, run.State)
		}
		wantIDs := append([]int64(nil), idsByState[state]...)
		reverseInt64s(wantIDs)
		assert.Equal(wantIDs, gotIDs, state)
	}

	queued, err := store.ListSourceOperationRunsForTest(st, t.Context(), operations.Query{
		States: []operations.State{operations.StateQueued}, Limit: 100,
	})
	require.NoError(err)
	assert.Empty(queued)
}

func TestOperationRunsPersonSweepProjectsStatesTriggersCountersAndFailures(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	base := time.Date(2026, 8, 28, 15, 0, 0, 123_000_000, time.UTC)
	classes := []peoplesweep.FailureClass{
		peoplesweep.FailurePolicy, peoplesweep.FailureBudget, peoplesweep.FailureLeaseLost,
		peoplesweep.FailureRateLimited, peoplesweep.FailureTimeout,
		peoplesweep.FailureProviderHTTP, peoplesweep.FailureInvalidOutput,
		peoplesweep.FailureArchiveGap, peoplesweep.FailureInternal, "",
	}
	wantCodes := []operations.PublicErrorCode{
		operations.PublicErrorPolicy, operations.PublicErrorBudget, operations.PublicErrorLeaseLost,
		operations.PublicErrorRateLimited, operations.PublicErrorTimeout,
		operations.PublicErrorProviderHTTP, operations.PublicErrorInvalidOutput,
		operations.PublicErrorArchiveGap, operations.PublicErrorInternal,
		operations.PublicErrorPersonSweepFailed,
	}

	running := insertPersonSweepOperationRun(t, st, personSweepOperationSeed{
		id: "run-running", trigger: "scheduled", state: "running", startedAt: base,
		attempted: 2, succeeded: 1, failed: 1, projectedWrites: 3,
	})
	succeeded := insertPersonSweepOperationRun(t, st, personSweepOperationSeed{
		id: "run-succeeded", trigger: "manual", state: "succeeded", startedAt: base.Add(time.Millisecond),
		attempted: 4, succeeded: 4, projectedWrites: 5,
	})

	for index, class := range classes {
		id := fmt.Sprintf("run-failure-%02d", index)
		state := "failed"
		if index%2 == 0 {
			state = "partial"
		}
		insertPersonSweepOperationRun(t, st, personSweepOperationSeed{
			id: id, trigger: "manual", state: state,
			startedAt: base.Add(time.Duration(index+2) * time.Millisecond),
			attempted: 7, succeeded: 2, failed: 5, projectedWrites: 6,
		})
		if class != "" {
			insertPersonSweepOperationAttempt(t, st, id, fmt.Sprintf("attempt-%02d", index), class,
				base.Add(time.Duration(index+2)*time.Millisecond))
		}
		run, err := store.GetPersonSweepOperationRunForTest(
			st, t.Context(), mustPersonSweepOperationID(t, id))
		require.NoError(err)
		assert.Equal(operations.State(state), run.State)
		require.NotNil(run.Error)
		assert.Equal(wantCodes[index], run.Error.Code)
		assert.Equal(operations.TriggerManual, *run.Trigger)
		assert.Equal([]operations.PublicCounter{
			{Name: operations.CounterAttempted, Unit: operations.CounterUnitPeople, Value: 7},
			{Name: operations.CounterSucceeded, Unit: operations.CounterUnitPeople, Value: 2},
			{Name: operations.CounterFailed, Unit: operations.CounterUnitPeople, Value: 5},
			{Name: operations.CounterProjectedWrites, Unit: operations.CounterUnitWrites, Value: 6},
		}, run.Counters)
		require.NoError(run.Validate())
	}

	runningRun, err := store.GetPersonSweepOperationRunForTest(
		st, t.Context(), mustPersonSweepOperationID(t, running))
	require.NoError(err)
	assert.Equal(operations.StateRunning, runningRun.State)
	assert.Equal(operations.TriggerScheduled, *runningRun.Trigger)
	assert.Nil(runningRun.FinishedAt)
	assert.Nil(runningRun.Error)

	succeededRun, err := store.GetPersonSweepOperationRunForTest(
		st, t.Context(), mustPersonSweepOperationID(t, succeeded))
	require.NoError(err)
	assert.Equal(operations.StateSucceeded, succeededRun.State)
	assert.Equal(operations.TriggerManual, *succeededRun.Trigger)
	require.NotNil(succeededRun.FinishedAt)
	assert.Equal(succeededRun.StartedAt.Add(time.Second), *succeededRun.FinishedAt)
	assert.Nil(succeededRun.Error)

	for _, state := range []operations.State{
		operations.StateRunning, operations.StateSucceeded,
		operations.StatePartial, operations.StateFailed,
	} {
		runs, listErr := store.ListPersonSweepOperationRunsForTest(st, t.Context(), operations.Query{
			States: []operations.State{state}, Limit: 100,
		})
		require.NoError(listErr)
		for _, run := range runs {
			assert.Equal(state, run.State)
		}
	}
	queued, err := store.ListPersonSweepOperationRunsForTest(st, t.Context(), operations.Query{
		States: []operations.State{operations.StateCancelled, operations.StateQueued}, Limit: 100,
	})
	require.NoError(err)
	assert.Empty(queued)
}

func TestOperationRunsPersonSweepPagesMillisecondsAndTextIDs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	instant := time.Date(2026, 8, 28, 16, 0, 0, 987_000_000, time.UTC)
	for _, id := range []string{"run-Z", "run-ä", "run-a"} {
		insertPersonSweepOperationRun(t, st, personSweepOperationSeed{
			id: id, trigger: "manual", state: "succeeded", startedAt: instant,
		})
	}
	var got []string
	var position *operations.Position
	for {
		runs, err := store.ListPersonSweepOperationRunsForTest(st, t.Context(), operations.Query{
			Position: position, Limit: 1,
		})
		require.NoError(err)
		if len(runs) == 0 {
			break
		}
		require.Len(runs, 1)
		id, ok := runs[0].ID.Text()
		require.True(ok)
		got = append(got, id)
		position = &operations.Position{StartedAt: runs[0].StartedAt, ID: runs[0].ID}
	}
	assert.Equal([]string{"run-ä", "run-a", "run-Z"}, got,
		"public ordering is bytewise and cannot follow a locale collation")

	sourceID, err := operations.NewInt64ID(operations.KindSourceSync, 99)
	require.NoError(err)
	afterSource, err := store.ListPersonSweepOperationRunsForTest(st, t.Context(), operations.Query{
		Position: &operations.Position{StartedAt: instant, ID: sourceID}, Limit: 10,
	})
	require.NoError(err)
	assert.Empty(afterSource, "person_sweep sorts before source_sync at the same instant")
}

func TestOperationRunsPersonSweepUsesNewestNonemptyFailureClass(t *testing.T) {
	st := testutil.NewTestStore(t)
	startedAt := time.Date(2026, 8, 28, 16, 30, 0, 0, time.UTC)
	runID := insertPersonSweepOperationRun(t, st, personSweepOperationSeed{
		id: "run-failure-order", trigger: "manual", state: "failed", startedAt: startedAt,
		attempted: 4, failed: 4,
	})
	insertPersonSweepOperationAttempt(t, st, runID, "attempt-old",
		peoplesweep.FailureTimeout, startedAt)
	insertPersonSweepOperationAttempt(t, st, runID, "attempt-newer-empty",
		"", startedAt.Add(time.Second))
	insertPersonSweepOperationAttempt(t, st, runID, "attempt-Z",
		peoplesweep.FailureInvalidOutput, startedAt.Add(2*time.Second))
	insertPersonSweepOperationAttempt(t, st, runID, "attempt-ä",
		peoplesweep.FailurePolicy, startedAt.Add(2*time.Second))

	run, err := store.GetPersonSweepOperationRunForTest(
		st, t.Context(), mustPersonSweepOperationID(t, runID))
	require.NoError(t, err)
	require.NotNil(t, run.Error)
	assert.Equal(t, operations.PublicErrorPolicy, run.Error.Code,
		"same-time failure ties use bytewise descending attempt ID")
}

func TestOperationRunsPersonSweepListDetailStatusAndPrivacy(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	base := time.Date(2026, 8, 28, 17, 0, 0, 456_000_000, time.UTC)
	successID := insertPersonSweepOperationRun(t, st, personSweepOperationSeed{
		id: "private-success", trigger: "scheduled", state: "succeeded", startedAt: base,
	})
	failedID := insertPersonSweepOperationRun(t, st, personSweepOperationSeed{
		id: "private-failed", trigger: "manual", state: "failed", startedAt: base.Add(time.Millisecond),
		attempted: 1, failed: 1,
	})
	insertPersonSweepOperationAttempt(t, st, failedID, "private-attempt-id",
		peoplesweep.FailureTimeout, base.Add(time.Millisecond))
	runningID := insertPersonSweepOperationRun(t, st, personSweepOperationSeed{
		id: "private-running", trigger: "scheduled", state: "running", startedAt: base.Add(2 * time.Millisecond),
	})

	runs, err := store.ListPersonSweepOperationRunsForTest(st, t.Context(), operations.Query{Limit: 10})
	require.NoError(err)
	require.Len(runs, 3)
	detail, err := store.GetPersonSweepOperationRunForTest(st, t.Context(), mustPersonSweepOperationID(t, failedID))
	require.NoError(err)
	assert.Equal(runs[1], detail)
	_, err = store.GetPersonSweepOperationRunForTest(st, t.Context(), mustPersonSweepOperationID(t, "missing"))
	require.ErrorIs(err, store.ErrOperationRunNotFound)

	status, err := store.PersonSweepOperationLaneStatusForTest(st, t.Context())
	require.NoError(err)
	require.NotNil(status.Active)
	require.NotNil(status.Latest)
	require.NotNil(status.LatestSuccessful)
	assert.Equal(runningID, personSweepOperationTextID(t, status.Active.ID))
	assert.Equal(runningID, personSweepOperationTextID(t, status.Latest.ID))
	assert.Equal(successID, personSweepOperationTextID(t, status.LatestSuccessful.ID))
	require.NoError(status.Validate())

	projected, err := json.Marshal(struct { //nolint:musttag // Marshal the public operation projection to scan for private fields.
		Runs   []operations.Run             `json:"runs"`
		Detail operations.Run               `json:"detail"`
		Status operations.LaneHistoryStatus `json:"status"`
	}{runs, detail, status})
	require.NoError(err)
	for _, marker := range []string{
		"private-person-id", "private-cursor-envelope", "private-program-fingerprint",
		"private-catalog-fingerprint", "private-provider-fingerprint", "private-evidence",
		"private-attempt-id", "private-provider-request", "private-model", "987654321",
	} {
		assert.NotContains(string(projected), marker)
	}

	_, err = st.DB().ExecContext(t.Context(), `DROP TABLE person_sweep_batches`)
	require.NoError(err)
	withoutBatches, err := store.ListPersonSweepOperationRunsForTest(st, t.Context(), operations.Query{Limit: 10})
	require.NoError(err, "projection must not materialize provider batches")
	assert.Equal(runs, withoutBatches)
}

type personSweepOperationSeed struct {
	id              string
	trigger         string
	state           string
	startedAt       time.Time
	attempted       int64
	succeeded       int64
	failed          int64
	projectedWrites int64
}

func insertPersonSweepOperationRun(t *testing.T, st *store.Store, seed personSweepOperationSeed) string {
	t.Helper()
	var completedAt any
	if seed.state != "running" {
		completedAt = personSweepTimestampParam(st, seed.startedAt.Add(time.Second))
	}
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`
		INSERT INTO person_sweep_runs (
			id, kind, mode, status, program_fingerprint, catalog_fingerprint,
			provider_fingerprint, attempt_count, success_count, failure_count,
			projected_write_count, actual_requests, actual_input_tokens,
			actual_output_tokens, actual_cost_micro_usd, started_at, completed_at
		) VALUES (?, ?, 'incremental', ?, ?, ?, ?, ?, ?, ?, ?, 77, 987654321, 88, 99, ?, ?)`),
		seed.id, seed.trigger, seed.state, "private-program-fingerprint",
		"private-catalog-fingerprint", "private-provider-fingerprint", seed.attempted,
		seed.succeeded, seed.failed, seed.projectedWrites,
		personSweepTimestampParam(st, seed.startedAt), completedAt)
	require.NoError(t, err)
	return seed.id
}

func insertPersonSweepOperationAttempt(
	t *testing.T, st *store.Store, runID, attemptID string,
	class peoplesweep.FailureClass, startedAt time.Time,
) {
	t.Helper()
	var personID int64
	err := st.DB().QueryRowContext(t.Context(), st.Rebind(`
		INSERT INTO persons (vcard_uid, display_name) VALUES (?, 'private-evidence') RETURNING id`),
		"private-person-id-"+attemptID).Scan(&personID)
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		INSERT INTO person_sweep_attempts (
			id, run_id, person_id, lease_fence, mode, status, failure_class,
			cursor_envelope_json, envelope_hash, program_fingerprint, catalog_fingerprint,
			provider_fingerprint, generation_key, provider_request_id, input_tokens,
			output_tokens, estimated_cost_micro_usd, started_at, completed_at
		) VALUES (?, ?, ?, 1, 'incremental', 'failed', ?, ?, ?, ?, ?, ?, ?, ?, 987654321, 88, 99, ?, ?)`),
		attemptID, runID, personID, class, `[{"marker":"private-cursor-envelope"}]`,
		"private-envelope-hash", "private-program-fingerprint", "private-catalog-fingerprint",
		"private-provider-fingerprint", "private-model", "private-provider-request",
		personSweepTimestampParam(st, startedAt), personSweepTimestampParam(st, startedAt.Add(time.Second)))
	require.NoError(t, err)
}

func personSweepTimestampParam(st *store.Store, value time.Time) any {
	if st.IsPostgreSQL() {
		return value.UTC()
	}
	return value.UTC().Format("2006-01-02 15:04:05.000")
}

func mustPersonSweepOperationID(t *testing.T, value string) operations.StableID {
	t.Helper()
	id, err := operations.NewTextID(operations.KindPersonSweep, value)
	require.NoError(t, err)
	return id
}

func personSweepOperationTextID(t *testing.T, id operations.StableID) string {
	t.Helper()
	value, ok := id.Text()
	require.True(t, ok)
	return value
}

func TestOperationRunsSourcePagesWholeSecondExactly(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source := createOperationSource(t, st, "paging-source@example.invalid")
	instant := time.Date(2026, 8, 28, 12, 34, 56, 0, time.UTC)
	lowerID := insertSourceOperationRun(t, st, source.ID, sourceOperationRunSeed{
		startedAt: instant, state: "completed", processed: 1,
	})
	higherID := insertSourceOperationRun(t, st, source.ID, sourceOperationRunSeed{
		startedAt: instant, state: "completed", processed: 2,
	})
	require.Greater(higherID, lowerID)

	first, err := store.ListSourceOperationRunsForTest(
		st, t.Context(), operations.Query{Limit: 1})
	require.NoError(err)
	require.Len(first, 1)
	assert.Equal(higherID, sourceOperationIntID(t, first[0].ID))

	position := &operations.Position{StartedAt: first[0].StartedAt, ID: first[0].ID}
	second, err := store.ListSourceOperationRunsForTest(
		st, t.Context(), operations.Query{Position: position, Limit: 10})
	require.NoError(err)
	require.Len(second, 1)
	assert.Equal(lowerID, sourceOperationIntID(t, second[0].ID))

	lastPosition := &operations.Position{StartedAt: second[0].StartedAt, ID: second[0].ID}
	afterLast, err := store.ListSourceOperationRunsForTest(
		st, t.Context(), operations.Query{Position: lastPosition, Limit: 10})
	require.NoError(err)
	assert.Empty(afterLast)

	cardDAVCursor, err := operations.NewInt64ID(operations.KindCardDAVSync, 99)
	require.NoError(err)
	afterEarlierKind, err := store.ListSourceOperationRunsForTest(st, t.Context(), operations.Query{
		Position: &operations.Position{StartedAt: instant, ID: cardDAVCursor}, Limit: 10,
	})
	require.NoError(err)
	require.Len(afterEarlierKind, 2)
	assert.Equal([]int64{higherID, lowerID}, []int64{
		sourceOperationIntID(t, afterEarlierKind[0].ID),
		sourceOperationIntID(t, afterEarlierKind[1].ID),
	})
}

func TestOperationRunsSourceListDetailNotFoundAndStatus(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	emptyStatus, err := store.SourceOperationLaneStatusForTest(st, t.Context())
	require.NoError(err)
	assert.Equal(operations.LaneHistoryStatus{
		Kind:                operations.KindSourceSync,
		Lane:                operations.LaneMessages,
		HistoryAvailability: operations.HistoryAvailable,
	}, emptyStatus)

	source := createOperationSource(t, st, "status-source@example.invalid")
	base := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	succeededID := insertSourceOperationRun(t, st, source.ID, sourceOperationRunSeed{
		startedAt: base, state: "completed", processed: 3, added: 1,
	})
	failedID := insertSourceOperationRun(t, st, source.ID, sourceOperationRunSeed{
		startedAt: base.Add(time.Minute), state: "failed", processed: 2,
	})
	runningID := insertSourceOperationRun(t, st, source.ID, sourceOperationRunSeed{
		startedAt: base.Add(2 * time.Minute), state: "running", processed: 1,
	})

	limited, err := store.ListSourceOperationRunsForTest(
		st, t.Context(), operations.Query{Limit: 2})
	require.NoError(err)
	require.Len(limited, 2)
	assert.Equal([]int64{runningID, failedID}, []int64{
		sourceOperationIntID(t, limited[0].ID), sourceOperationIntID(t, limited[1].ID),
	})

	detail, err := store.GetSourceOperationRunForTest(st, t.Context(), limited[1].ID)
	require.NoError(err)
	assert.Equal(limited[1], detail)

	_, err = store.GetSourceOperationRunForTest(
		st, t.Context(), mustSourceOperationID(t, runningID+1000))
	require.ErrorIs(err, store.ErrOperationRunNotFound)
	_, err = store.ListSourceOperationRunsForTest(
		st, t.Context(), operations.Query{Limit: 0})
	require.Error(err)

	status, err := store.SourceOperationLaneStatusForTest(st, t.Context())
	require.NoError(err)
	require.NotNil(status.Active)
	require.NotNil(status.Latest)
	require.NotNil(status.LatestSuccessful)
	assert.Equal(runningID, sourceOperationIntID(t, status.Active.ID))
	assert.Equal(runningID, sourceOperationIntID(t, status.Latest.ID))
	assert.Equal(succeededID, sourceOperationIntID(t, status.LatestSuccessful.ID))
	require.NoError(status.Validate())
}

func TestOperationRunsSourceDoesNotHydratePrivateRows(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	sourceMarker := "private-source-identifier@example.invalid"
	errorMarker := "private-run-error-marker"
	beforeMarker := "private-provider-cursor-before"
	afterMarker := "private-provider-cursor-after"
	itemIDMarker := "private-source-message-id"
	itemErrorMarker := "private-item-error-detail"
	source := createOperationSource(t, st, sourceMarker)
	id := insertSourceOperationRun(t, st, source.ID, sourceOperationRunSeed{
		startedAt:    time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC),
		state:        "failed",
		processed:    1,
		errorMessage: errorMarker,
		cursorBefore: beforeMarker,
		cursorAfter:  afterMarker,
	})
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`
		INSERT INTO sync_run_items (
			sync_run_id, source_message_id, phase, status, error_kind, error_message
		) VALUES (?, ?, 'fetch', 'error', 'synthetic', ?)`), id, itemIDMarker, itemErrorMarker)
	require.NoError(err)

	runs, err := store.ListSourceOperationRunsForTest(
		st, t.Context(), operations.Query{Limit: 10})
	require.NoError(err)
	require.Len(runs, 1)
	detail, err := store.GetSourceOperationRunForTest(
		st, t.Context(), mustSourceOperationID(t, id))
	require.NoError(err)
	status, err := store.SourceOperationLaneStatusForTest(st, t.Context())
	require.NoError(err)

	projected, err := json.Marshal(struct { //nolint:musttag // Marshal the public operation projection to scan for private fields.
		Runs   []operations.Run             `json:"runs"`
		Detail operations.Run               `json:"detail"`
		Status operations.LaneHistoryStatus `json:"status"`
	}{Runs: runs, Detail: detail, Status: status})
	require.NoError(err)
	for _, marker := range []string{
		sourceMarker, errorMarker, beforeMarker, afterMarker, itemIDMarker, itemErrorMarker,
	} {
		assert.NotContains(string(projected), marker)
	}
	assert.Equal(sourceOperationPublicError(), detail.Error)

	_, err = st.DB().ExecContext(t.Context(), `DROP TABLE sync_run_items`)
	require.NoError(err)
	withoutItems, err := store.ListSourceOperationRunsForTest(
		st, t.Context(), operations.Query{Limit: 10})
	require.NoError(err, "the safe projection must not query sync_run_items")
	assert.Equal(runs, withoutItems)
	withoutItemsDetail, err := store.GetSourceOperationRunForTest(
		st, t.Context(), mustSourceOperationID(t, id))
	require.NoError(err, "detail must not hydrate sync_run_items")
	assert.Equal(detail, withoutItemsDetail)
}

func TestOperationRunsSourceRejectsInvalidDurableRows(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source := createOperationSource(t, st, "invalid-source@example.invalid")
	base := time.Date(2026, 8, 28, 8, 0, 0, 0, time.UTC)
	validID := insertSourceOperationRun(t, st, source.ID, sourceOperationRunSeed{
		startedAt: base, state: "completed", processed: 1,
	})
	invalidID := insertSourceOperationRun(t, st, source.ID, sourceOperationRunSeed{
		startedAt: base.Add(time.Minute), state: "unknown", processed: 1,
	})

	_, err := store.GetSourceOperationRunForTest(
		st, t.Context(), mustSourceOperationID(t, invalidID))
	require.Error(err, "unknown durable states must fail closed")
	runs, err := store.ListSourceOperationRunsForTest(
		st, t.Context(), operations.Query{Limit: 10})
	require.Error(err, "one invalid selected row must fail the whole list")
	assert.Empty(runs)

	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		UPDATE sync_runs SET status = 'completed', messages_processed = -1 WHERE id = ?`), invalidID)
	require.NoError(err)
	_, err = store.GetSourceOperationRunForTest(
		st, t.Context(), mustSourceOperationID(t, invalidID))
	require.Error(err, "negative progress must fail closed")

	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		UPDATE sync_runs SET messages_processed = 1, errors_count = -1 WHERE id = ?`), invalidID)
	require.NoError(err)
	_, err = store.GetSourceOperationRunForTest(
		st, t.Context(), mustSourceOperationID(t, invalidID))
	require.Error(err, "negative item errors must fail closed")

	valid, err := store.GetSourceOperationRunForTest(
		st, t.Context(), mustSourceOperationID(t, validID))
	require.NoError(err)
	assert.Equal(operations.StateSucceeded, valid.State)
}

func TestOperationRunsSourceRejectsMalformedSQLiteTimestamps(t *testing.T) {
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	source := createOperationSource(t, st, "timestamp-source@example.invalid")
	started := time.Date(2026, 8, 28, 7, 0, 0, 0, time.UTC)
	runningID := insertSourceOperationRun(t, st, source.ID, sourceOperationRunSeed{
		startedAt: started.Add(time.Minute), state: "running", processed: 1,
	})
	_, err := st.DB().ExecContext(t.Context(),
		`UPDATE sync_runs SET completed_at = 'malformed' WHERE id = ?`, runningID)
	require.NoError(err)
	_, err = store.GetSourceOperationRunForTest(
		st, t.Context(), mustSourceOperationID(t, runningID))
	require.Error(err, "a running row cannot hide a malformed non-null completion timestamp")

	id := insertSourceOperationRun(t, st, source.ID, sourceOperationRunSeed{
		startedAt: started, state: "completed", processed: 1,
	})

	_, err = st.DB().ExecContext(t.Context(), `UPDATE sync_runs SET started_at = 'malformed' WHERE id = ?`, id)
	require.NoError(err)
	_, err = store.GetSourceOperationRunForTest(
		st, t.Context(), mustSourceOperationID(t, id))
	require.Error(err, "malformed required timestamps must fail closed")

	_, err = st.DB().ExecContext(t.Context(), `
		UPDATE sync_runs SET started_at = ?, completed_at = 'malformed' WHERE id = ?`,
		started.UTC().Format("2006-01-02 15:04:05"), id)
	require.NoError(err)
	_, err = store.GetSourceOperationRunForTest(
		st, t.Context(), mustSourceOperationID(t, id))
	require.Error(err, "malformed terminal timestamps must fail closed")
}

type sourceOperationRunSeed struct {
	startedAt    time.Time
	state        string
	processed    int64
	added        int64
	updated      int64
	itemErrors   int64
	errorMessage string
	cursorBefore string
	cursorAfter  string
}

func createOperationSource(t *testing.T, st *store.Store, identifier string) *store.Source {
	t.Helper()
	source, err := st.GetOrCreateSource("gmail", identifier)
	require.NoError(t, err)
	return source
}

func insertSourceOperationRun(
	t *testing.T, st *store.Store, sourceID int64, seed sourceOperationRunSeed,
) int64 {
	t.Helper()
	var completedAt any
	if seed.state != "running" {
		completedAt = sourceOperationTimestampParam(st, seed.startedAt.Add(time.Second))
	}
	var id int64
	err := st.DB().QueryRowContext(t.Context(), st.Rebind(`
		INSERT INTO sync_runs (
			source_id, started_at, completed_at, status,
			messages_processed, messages_added, messages_updated, errors_count,
			error_message, cursor_before, cursor_after
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''))
		RETURNING id`),
		sourceID, sourceOperationTimestampParam(st, seed.startedAt), completedAt, seed.state,
		seed.processed, seed.added, seed.updated, seed.itemErrors,
		seed.errorMessage, seed.cursorBefore, seed.cursorAfter,
	).Scan(&id)
	require.NoError(t, err)
	return id
}

func sourceOperationTimestampParam(st *store.Store, value time.Time) any {
	if st.IsPostgreSQL() {
		return value.UTC()
	}
	return value.UTC().Format("2006-01-02 15:04:05")
}

func mustSourceOperationID(t *testing.T, id int64) operations.StableID {
	t.Helper()
	stableID, err := operations.NewInt64ID(operations.KindSourceSync, id)
	require.NoError(t, err)
	return stableID
}

func sourceOperationIntID(t *testing.T, id operations.StableID) int64 {
	t.Helper()
	value, ok := id.Int64()
	require.True(t, ok)
	return value
}

func sourceOperationPublicError() *operations.PublicError {
	return &operations.PublicError{
		Code: operations.PublicErrorSourceSyncFailed, Message: "Source sync failed.",
	}
}

func reverseInt64s(values []int64) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func TestOperationRunsCardDAVProjectsNativeStatesTriggersAndCounters(t *testing.T) {
	newAssertions := assert.New
	newRequirements := require.New
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		seed      cardDAVOperationRunSeed
		wantState operations.State
		wantError *operations.PublicError
	}{
		{
			name: "running manual",
			seed: cardDAVOperationRunSeed{
				trigger: store.CardDAVSyncTriggerManual, state: store.CardDAVSyncRunRunning,
				startedAt: base, books: 1,
			},
			wantState: operations.StateRunning,
		},
		{
			name: "succeeded scheduled",
			seed: cardDAVOperationRunSeed{
				trigger: store.CardDAVSyncTriggerScheduled, state: store.CardDAVSyncRunSucceeded,
				startedAt: base.Add(time.Minute), books: 2, created: 3, updated: 4, removed: 5,
			},
			wantState: operations.StateSucceeded,
		},
		{
			name: "partial",
			seed: cardDAVOperationRunSeed{
				trigger: store.CardDAVSyncTriggerManual, state: store.CardDAVSyncRunPartial,
				startedAt: base.Add(2 * time.Minute), books: 2, created: 1,
				errorCode: "sync_failed", errorMessage: "private-partial-message",
			},
			wantState: operations.StatePartial,
			wantError: &operations.PublicError{
				Code: operations.PublicErrorSyncFailed, Message: "CardDAV sync failed.",
			},
		},
		{
			name: "failed",
			seed: cardDAVOperationRunSeed{
				trigger: store.CardDAVSyncTriggerScheduled, state: store.CardDAVSyncRunFailed,
				startedAt: base.Add(3 * time.Minute), books: 1,
				errorCode: "authentication_failed", errorMessage: "private-failed-message",
			},
			wantState: operations.StateFailed,
			wantError: &operations.PublicError{
				Code: operations.PublicErrorAuthenticationFailed, Message: "CardDAV authentication failed.",
			},
		},
		{
			name: "cancelled",
			seed: cardDAVOperationRunSeed{
				trigger: store.CardDAVSyncTriggerManual, state: store.CardDAVSyncRunCancelled,
				startedAt: base.Add(4 * time.Minute), books: 1,
				errorCode: "cancelled", errorMessage: "private-cancelled-message",
			},
			wantState: operations.StateCancelled,
			wantError: &operations.PublicError{
				Code: operations.PublicErrorCancelled, Message: "CardDAV sync was cancelled.",
			},
		},
	}

	idsByState := make(map[operations.State]int64, len(tests))
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := newRequirements(t)
			assert := newAssertions(t)
			id := insertCardDAVOperationRun(t, st, test.seed)
			idsByState[test.wantState] = id
			got, err := store.GetCardDAVOperationRunForTest(
				st, t.Context(), mustCardDAVOperationID(t, id))
			require.NoError(err)
			require.NotNil(got.Trigger)
			assert.Equal(test.wantState, got.State)
			assert.Equal(test.wantError, got.Error)
			assert.Equal(operations.Trigger(test.seed.trigger), *got.Trigger)
			assert.Equal([]operations.PublicCounter{
				{Name: operations.CounterBooks, Unit: operations.CounterUnitBooks, Value: test.seed.books},
				{Name: operations.CounterCreated, Unit: operations.CounterUnitContacts, Value: test.seed.created},
				{Name: operations.CounterUpdated, Unit: operations.CounterUnitContacts, Value: test.seed.updated},
				{Name: operations.CounterRemoved, Unit: operations.CounterUnitContacts, Value: test.seed.removed},
			}, got.Counters)
			require.NoError(got.Validate())
		})
	}

	for state, wantID := range idsByState {
		runs, err := store.ListCardDAVOperationRunsForTest(st, t.Context(), operations.Query{
			States: []operations.State{state}, Limit: 10,
		})
		require.NoError(err)
		require.Len(runs, 1)
		assert.Equal(wantID, cardDAVOperationIntID(t, runs[0].ID))
		assert.Equal(state, runs[0].State)
	}
	queued, err := store.ListCardDAVOperationRunsForTest(st, t.Context(), operations.Query{
		States: []operations.State{operations.StateQueued}, Limit: 10,
	})
	require.NoError(err)
	assert.Empty(queued)
}

func TestOperationRunsCardDAVProjectsOnlyFixedFailureMessages(t *testing.T) {
	st := testutil.NewTestStore(t)
	base := time.Date(2026, 8, 28, 13, 0, 0, 0, time.UTC)
	tests := []struct {
		code        string
		state       store.CardDAVSyncRunState
		wantCode    operations.PublicErrorCode
		wantMessage string
	}{
		{"cancelled", store.CardDAVSyncRunCancelled, operations.PublicErrorCancelled, "CardDAV sync was cancelled."},
		{"retry_after", store.CardDAVSyncRunFailed, operations.PublicErrorRetryAfter, "CardDAV sync is temporarily paused."},
		{"authentication_failed", store.CardDAVSyncRunFailed, operations.PublicErrorAuthenticationFailed, "CardDAV authentication failed."},
		{"upstream_failed", store.CardDAVSyncRunFailed, operations.PublicErrorUpstreamFailed, "CardDAV server request failed."},
		{"safety_limit", store.CardDAVSyncRunFailed, operations.PublicErrorSafetyLimit, "CardDAV sync exceeded its safety limits."},
		{"sync_failed", store.CardDAVSyncRunFailed, operations.PublicErrorSyncFailed, "CardDAV sync failed."},
		{"unsafe_error_redacted", store.CardDAVSyncRunFailed, operations.PublicErrorUnsafeErrorRedacted, "CardDAV sync failed; sensitive details were removed."},
		{"daemon_restarted", store.CardDAVSyncRunFailed, operations.PublicErrorDaemonRestarted, "CardDAV sync stopped because the daemon restarted."},
	}
	for index, test := range tests {
		t.Run(test.code, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			privateMessage := "private-stored-message-" + test.code
			id := insertCardDAVOperationRun(t, st, cardDAVOperationRunSeed{
				trigger: store.CardDAVSyncTriggerManual,
				state:   test.state, startedAt: base.Add(time.Duration(index) * time.Minute),
				errorCode: test.code, errorMessage: privateMessage,
			})
			run, err := store.GetCardDAVOperationRunForTest(
				st, t.Context(), mustCardDAVOperationID(t, id))
			require.NoError(err)
			require.NotNil(run.Error)
			assert.Equal(test.wantCode, run.Error.Code)
			assert.Equal(test.wantMessage, run.Error.Message)
			assert.NotContains(fmt.Sprintf("%#v", run), privateMessage)
		})
	}

	privateMarker := "credential=private-unknown-code-marker"
	unknownID := insertCardDAVOperationRun(t, st, cardDAVOperationRunSeed{
		trigger: store.CardDAVSyncTriggerScheduled, state: store.CardDAVSyncRunFailed,
		startedAt: base.Add(20 * time.Minute), errorCode: "new_safe_code", errorMessage: privateMarker,
	})
	unknown, err := store.GetCardDAVOperationRunForTest(
		st, t.Context(), mustCardDAVOperationID(t, unknownID))
	require.NoError(t, err)
	assert.Equal(t, &operations.PublicError{
		Code: operations.PublicErrorCardDAVSyncFailed, Message: "CardDAV sync failed.",
	}, unknown.Error)
	assert.NotContains(t, fmt.Sprintf("%#v", unknown), privateMarker)
}

func TestOperationRunsCardDAVPagesSameSecondByNumericID(t *testing.T) {
	st := testutil.NewTestStore(t)
	instant := time.Date(2026, 8, 28, 14, 15, 16, 0, time.UTC)
	for range 10 {
		insertCardDAVOperationRun(t, st, cardDAVOperationRunSeed{
			trigger: store.CardDAVSyncTriggerManual, state: store.CardDAVSyncRunSucceeded,
			startedAt: instant,
		})
	}

	var got []int64
	var position *operations.Position
	for {
		page, err := store.ListCardDAVOperationRunsForTest(st, t.Context(), operations.Query{
			Position: position, Limit: 1,
		})
		require.NoError(t, err)
		if len(page) == 0 {
			break
		}
		require.Len(t, page, 1)
		got = append(got, cardDAVOperationIntID(t, page[0].ID))
		position = &operations.Position{StartedAt: page[0].StartedAt, ID: page[0].ID}
	}
	assert.Equal(t, []int64{10, 9, 8, 7, 6, 5, 4, 3, 2, 1}, got)
}

func TestOperationRunsCardDAVPagesAfterFractionalCrossKindCursor(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	instant := time.Date(2026, 8, 28, 14, 15, 16, 0, time.UTC)
	lowerID := insertCardDAVOperationRun(t, st, cardDAVOperationRunSeed{
		trigger: store.CardDAVSyncTriggerManual, state: store.CardDAVSyncRunSucceeded,
		startedAt: instant,
	})
	higherID := insertCardDAVOperationRun(t, st, cardDAVOperationRunSeed{
		trigger: store.CardDAVSyncTriggerManual, state: store.CardDAVSyncRunSucceeded,
		startedAt: instant,
	})
	personCursorID, err := operations.NewTextID(operations.KindPersonSweep, "person-sweep-cursor")
	require.NoError(err)

	runs, err := store.ListCardDAVOperationRunsForTest(st, t.Context(), operations.Query{
		Position: &operations.Position{
			StartedAt: instant.Add(500 * time.Millisecond),
			ID:        personCursorID,
		},
		Limit: 1,
	})
	require.NoError(err)
	require.Len(runs, 1, "a whole-second CardDAV row is older than the fractional cursor")
	assert.Equal(higherID, cardDAVOperationIntID(t, runs[0].ID))

	second, err := store.ListCardDAVOperationRunsForTest(st, t.Context(), operations.Query{
		Position: &operations.Position{StartedAt: runs[0].StartedAt, ID: runs[0].ID},
		Limit:    1,
	})
	require.NoError(err)
	require.Len(second, 1)
	assert.Equal(lowerID, cardDAVOperationIntID(t, second[0].ID))

	afterLast, err := store.ListCardDAVOperationRunsForTest(st, t.Context(), operations.Query{
		Position: &operations.Position{StartedAt: second[0].StartedAt, ID: second[0].ID},
		Limit:    1,
	})
	require.NoError(err)
	assert.Empty(afterLast)
}

func TestOperationRunsCardDAVDetailAndStatusMatchListProjection(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	empty, err := store.CardDAVOperationLaneStatusForTest(st, t.Context())
	require.NoError(err)
	assert.Equal(operations.LaneHistoryStatus{
		Kind: operations.KindCardDAVSync, Lane: operations.LaneContacts,
		HistoryAvailability: operations.HistoryAvailable,
	}, empty)

	base := time.Date(2026, 8, 28, 15, 0, 0, 0, time.UTC)
	succeededID := insertCardDAVOperationRun(t, st, cardDAVOperationRunSeed{
		trigger: store.CardDAVSyncTriggerManual, state: store.CardDAVSyncRunSucceeded,
		startedAt: base, books: 1, created: 1,
	})
	insertCardDAVOperationRun(t, st, cardDAVOperationRunSeed{
		trigger: store.CardDAVSyncTriggerScheduled, state: store.CardDAVSyncRunFailed,
		startedAt: base.Add(time.Minute), errorCode: "upstream_failed", errorMessage: "private-status-message",
	})
	runningID := insertCardDAVOperationRun(t, st, cardDAVOperationRunSeed{
		trigger: store.CardDAVSyncTriggerManual, state: store.CardDAVSyncRunRunning,
		startedAt: base.Add(2 * time.Minute), books: 2,
	})

	list, err := store.ListCardDAVOperationRunsForTest(
		st, t.Context(), operations.Query{Limit: 10})
	require.NoError(err)
	require.Len(list, 3)
	detail, err := store.GetCardDAVOperationRunForTest(st, t.Context(), list[1].ID)
	require.NoError(err)
	assert.Equal(list[1], detail)

	status, err := store.CardDAVOperationLaneStatusForTest(st, t.Context())
	require.NoError(err)
	require.NotNil(status.Active)
	require.NotNil(status.Latest)
	require.NotNil(status.LatestSuccessful)
	assert.Equal(runningID, cardDAVOperationIntID(t, status.Active.ID))
	assert.Equal(list[0], *status.Latest)
	assert.Equal(succeededID, cardDAVOperationIntID(t, status.LatestSuccessful.ID))
	require.NoError(status.Validate())

	_, err = store.GetCardDAVOperationRunForTest(
		st, t.Context(), mustCardDAVOperationID(t, runningID+1000))
	require.ErrorIs(err, store.ErrOperationRunNotFound)
}

func TestOperationRunsCardDAVExcludesPrivateCardDAVData(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	privateMarkers := []string{
		"private-account-url-marker", "private-username-marker", "private-book-url-marker",
		"private-href-marker", "private-vcard-marker", "private-credential-marker",
		"private-remote-cursor-marker", "private-stored-message-marker",
	}
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL:      "https://private-account-url-marker.example.invalid/carddav/",
		Username:     "private-username-marker",
		PrincipalURL: "https://private-account-url-marker.example.invalid/principal/",
		HomeURL:      "https://private-account-url-marker.example.invalid/home/",
		Books: []store.CardDAVDiscoveredBook{{
			CanonicalURL: "https://private-book-url-marker.example.invalid/book/",
			DisplayName:  "Private book", DiscoveryIndex: 0,
		}},
	})
	require.NoError(err)
	require.Len(books, 1)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE carddav_address_books
		SET sync_token = ? WHERE id = ?`), "private-remote-cursor-marker", books[0].ID)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO carddav_resources (
		address_book_id, href, remote_uid, remote_etag, remote_body,
		remote_semantic_hash, local_hash, mapping_status, governance
	) VALUES (?, ?, ?, ?, ?, ?, ?, 'unbound', 'none')`),
		books[0].ID, "private-href-marker.vcf", "private-contact-uid", "private-etag",
		[]byte("BEGIN:VCARD\nFN:private-vcard-marker\nEND:VCARD"), "remote-hash", "local-hash")
	require.NoError(err)

	id := insertCardDAVOperationRun(t, st, cardDAVOperationRunSeed{
		trigger: store.CardDAVSyncTriggerManual, state: store.CardDAVSyncRunFailed,
		startedAt: time.Date(2026, 8, 28, 16, 0, 0, 0, time.UTC), full: true,
		errorCode:    "new_safe_code",
		errorMessage: "credential=private-credential-marker private-stored-message-marker",
	})
	runs, err := store.ListCardDAVOperationRunsForTest(
		st, t.Context(), operations.Query{Limit: 10})
	require.NoError(err)
	require.Len(runs, 1)
	detail, err := store.GetCardDAVOperationRunForTest(
		st, t.Context(), mustCardDAVOperationID(t, id))
	require.NoError(err)
	assert.Equal(runs[0], detail)
	status, err := store.CardDAVOperationLaneStatusForTest(st, t.Context())
	require.NoError(err)

	projected := strings.ToLower(fmt.Sprintf("%#v %#v %#v", runs, detail, status))
	for _, marker := range privateMarkers {
		assert.NotContains(projected, strings.ToLower(marker))
	}
	assert.NotContains(projected, "full:")
}

func TestOperationRunsCardDAVRejectsMalformedSQLiteFinishedTimestamp(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	st.DB().SetMaxOpenConns(1)
	runningID := insertCardDAVOperationRun(t, st, cardDAVOperationRunSeed{
		trigger: store.CardDAVSyncTriggerManual, state: store.CardDAVSyncRunRunning,
		startedAt: time.Date(2026, 8, 28, 17, 0, 0, 0, time.UTC),
	})

	_, err := st.DB().ExecContext(t.Context(), `PRAGMA ignore_check_constraints = ON`)
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(),
		`UPDATE carddav_sync_runs SET finished_at = 'malformed' WHERE id = ?`, runningID)
	require.NoError(t, err)

	_, err = store.GetCardDAVOperationRunForTest(
		st, t.Context(), mustCardDAVOperationID(t, runningID))
	require.Error(t, err, "a running row cannot hide a malformed non-null finish timestamp")
}

type cardDAVOperationRunSeed struct {
	trigger      store.CardDAVSyncTrigger
	full         bool
	state        store.CardDAVSyncRunState
	startedAt    time.Time
	books        int64
	created      int64
	updated      int64
	removed      int64
	errorCode    string
	errorMessage string
}

func insertCardDAVOperationRun(
	t *testing.T, st *store.Store, seed cardDAVOperationRunSeed,
) int64 {
	t.Helper()
	var finishedAt any
	if seed.state != store.CardDAVSyncRunRunning {
		finishedAt = cardDAVOperationTimestampArg(st, seed.startedAt.Add(time.Second))
	}
	var id int64
	err := st.DB().QueryRowContext(t.Context(), st.Rebind(`INSERT INTO carddav_sync_runs (
		trigger, full_sync, state, started_at, finished_at, books, created, updated, removed,
		error_code, error_message
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`),
		seed.trigger, seed.full, seed.state, cardDAVOperationTimestampArg(st, seed.startedAt),
		finishedAt, seed.books, seed.created, seed.updated, seed.removed,
		seed.errorCode, seed.errorMessage).Scan(&id)
	require.NoError(t, err)
	return id
}

func cardDAVOperationTimestampArg(st *store.Store, value time.Time) any {
	if st.IsPostgreSQL() {
		return value.UTC()
	}
	return value.UTC().Format("2006-01-02 15:04:05")
}

func mustCardDAVOperationID(t *testing.T, id int64) operations.StableID {
	t.Helper()
	stableID, err := operations.NewInt64ID(operations.KindCardDAVSync, id)
	require.NoError(t, err)
	return stableID
}

func cardDAVOperationIntID(t *testing.T, id operations.StableID) int64 {
	t.Helper()
	value, ok := id.Int64()
	require.True(t, ok)
	return value
}
