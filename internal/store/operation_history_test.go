package store_test

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

var allOperationHistoryKinds = []operations.Kind{
	operations.KindCardDAVSync,
	operations.KindDocumentEmbedding,
	operations.KindDocumentExtraction,
	operations.KindMessageEmbedding,
	operations.KindPersonEmbedding,
	operations.KindPersonEnrichment,
	operations.KindPersonSweep,
	operations.KindSourceSync,
	operations.KindVisualEmbedding,
}

func TestOperationHistorySnapshotReadsAllKindsInExactOrderAndDateBounds(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	startedAt := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	oldKeys := seedAllOperationHistoryKindsAt(t, st, startedAt, "old")
	wantKeys := seedAllOperationHistoryKindsAt(t, st, startedAt.Add(time.Second), "bounded")
	newKeys := seedAllOperationHistoryKindsAt(t, st, startedAt.Add(2*time.Second), "new")

	snapshot, err := st.ListRuns(t.Context(), operations.Query{Limit: 100})
	require.NoError(err)
	assert.Equal(allOperationHistoryKinds, st.Kinds())
	assert.Equal(allOperationHistoryKinds, snapshot.AvailableKinds)
	assert.Empty(snapshot.UnavailableKinds)
	assert.Equal(append(append(newKeys, wantKeys...), oldKeys...), operationRunKeys(t, snapshot.Runs),
		"neighboring timestamps sort newest first and identical timestamps sort by kind")
	assert.Equal(int64(3*len(allOperationHistoryKinds)), snapshot.MembershipRevision,
		"each visible history insert advances the shared membership revision")

	startedFrom := startedAt.Add(500 * time.Millisecond)
	startedBefore := startedAt.Add(1500 * time.Millisecond)
	bounded, err := st.ListRuns(t.Context(), operations.Query{
		StartedFrom: &startedFrom, StartedBefore: &startedBefore, Limit: 100,
	})
	require.NoError(err)
	assert.Equal(wantKeys, operationRunKeys(t, bounded.Runs),
		"sub-second half-open bounds agree across all adapter timestamp representations")

	limited, err := st.ListRuns(t.Context(), operations.Query{Limit: 3})
	require.NoError(err)
	assert.Len(limited.Runs, 4, "mixed history returns at most limit plus one")
}

func TestOperationAdapterPersonEnrichmentStartsAtFirstClaimAndKeepsOriginalOrder(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	requestedAt := time.Date(2026, 8, 29, 10, 30, 0, 0, time.UTC)
	queuedID := insertPersonEnrichmentHistoryRun(t, st, personEnrichmentHistorySeed{
		requestedAt: requestedAt,
		state:       "queued",
	})

	beforeClaim, err := st.ListRuns(t.Context(), operations.Query{
		Kinds: []operations.Kind{operations.KindPersonEnrichment}, Limit: 10,
	})
	require.NoError(err)
	assert.Empty(beforeClaim.Runs, "unclaimed enrichment requests are not operation history")

	originalStart := requestedAt.Add(time.Minute)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		UPDATE person_enrichment_runs SET started_at = ? WHERE id = ?`), originalStart, queuedID)
	require.NoError(err)
	claimed, err := st.ListRuns(t.Context(), operations.Query{
		Kinds: []operations.Kind{operations.KindPersonEnrichment}, Limit: 10,
	})
	require.NoError(err)
	require.Len(claimed.Runs, 1)
	assert.Equal(operations.StateQueued, claimed.Runs[0].State)
	assert.Equal(originalStart, claimed.Runs[0].StartedAt)

	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		UPDATE person_enrichment_runs SET state = 'running' WHERE id = ?`), queuedID)
	require.NoError(err)
	recovered, err := st.ListRuns(t.Context(), operations.Query{
		Kinds: []operations.Kind{operations.KindPersonEnrichment}, Limit: 10,
	})
	require.NoError(err)
	require.Len(recovered.Runs, 1)
	assert.Equal(operations.StateRunning, recovered.Runs[0].State)
	assert.Equal(originalStart, recovered.Runs[0].StartedAt,
		"recovery transitions do not replace the first claim order")
}

func TestOperationAdapterFailureRollsBackToSavepointAndReturnsPartialSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	startedAt := time.Date(2026, 8, 29, 11, 0, 0, 0, time.UTC)
	seedAllOperationHistoryKinds(t, st, startedAt)

	_, err := st.DB().ExecContext(t.Context(),
		`ALTER TABLE person_sweep_runs RENAME TO person_sweep_runs_unavailable`)
	require.NoError(err)

	snapshot, err := st.ListRuns(t.Context(), operations.Query{Limit: 100})
	require.NoError(err)
	assert.Equal([]operations.Kind{operations.KindPersonSweep}, snapshot.UnavailableKinds)
	wantAvailable := slices.DeleteFunc(slices.Clone(allOperationHistoryKinds), func(kind operations.Kind) bool {
		return kind == operations.KindPersonSweep
	})
	assert.Equal(wantAvailable, snapshot.AvailableKinds)
	assert.Len(snapshot.Runs, len(wantAvailable))
	assert.Contains(operationRunKeys(t, snapshot.Runs), "source_sync:1",
		"the adapter after the failed PostgreSQL statement remains readable")
	assert.Positive(snapshot.MembershipRevision)
}

func TestOperationAdapterValidationFailureReturnsPartialSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	startedAt := time.Date(2026, 8, 29, 11, 15, 0, 0, time.UTC)
	seedAllOperationHistoryKinds(t, st, startedAt)

	_, err := st.DB().ExecContext(t.Context(), `UPDATE sync_runs SET status = 'corrupt'`)
	require.NoError(err)

	snapshot, err := st.ListRuns(t.Context(), operations.Query{Limit: 100})
	require.NoError(err)
	assert.Equal([]operations.Kind{operations.KindSourceSync}, snapshot.UnavailableKinds)
	wantAvailable := slices.DeleteFunc(slices.Clone(allOperationHistoryKinds), func(kind operations.Kind) bool {
		return kind == operations.KindSourceSync
	})
	assert.Equal(wantAvailable, snapshot.AvailableKinds)
	assert.Len(snapshot.Runs, len(wantAvailable),
		"a durable decode failure is isolated to its adapter savepoint")
}

func TestOperationAdapterDispatchesListGetAndStatusForAllKinds(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	startedAt := time.Date(2026, 8, 29, 11, 30, 0, 0, time.UTC)
	seedAllOperationHistoryKinds(t, st, startedAt)

	snapshot, err := st.ListRuns(t.Context(), operations.Query{Limit: 100})
	require.NoError(err)
	require.Len(snapshot.Runs, len(allOperationHistoryKinds))
	for _, listed := range snapshot.Runs {
		detail, detailErr := st.GetRun(t.Context(), listed.ID)
		require.NoError(detailErr, listed.ID.Kind())
		assert.Equal(listed, detail)

		status, statusErr := st.LaneStatus(t.Context(), listed.ID.Kind())
		require.NoError(statusErr, listed.ID.Kind())
		assert.Equal(listed.ID.Kind(), status.Kind)
		require.NotNil(status.Latest)
		assert.Equal(listed.ID, status.Latest.ID)
		if listed.State == operations.StateRunning {
			require.NotNil(status.Active)
			assert.Equal(listed.ID, status.Active.ID)
		} else {
			require.NotNil(status.LatestSuccessful)
			assert.Equal(listed.ID, status.LatestSuccessful.ID)
		}
	}
}

func TestOperationAdapterStatusReturnsActiveLatestAndLatestSuccessfulRoles(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	base := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	succeeded := beginOperationHistoryInvocation(t, st, operations.KindMessageEmbedding, "status:succeeded", base)
	finishOperationHistoryInvocation(t, st, succeeded, base.Add(time.Minute),
		"succeeded", 1, 1, 0, "")
	failed := beginOperationHistoryInvocation(t, st, operations.KindMessageEmbedding, "status:failed", base.Add(time.Hour))
	finishOperationHistoryInvocation(t, st, failed, base.Add(time.Hour+time.Minute),
		"failed", 1, 0, 1, string(operations.PublicErrorInvocationInternal))
	running := beginOperationHistoryInvocation(t, st, operations.KindMessageEmbedding, "status:running", base.Add(2*time.Hour))

	status, err := st.LaneStatus(t.Context(), operations.KindMessageEmbedding)
	require.NoError(err)
	require.NotNil(status.Active)
	require.NotNil(status.Latest)
	require.NotNil(status.LatestSuccessful)
	assert.Equal(running, status.Active.ID)
	assert.Equal(running, status.Latest.ID)
	assert.Equal(succeeded, status.LatestSuccessful.ID)
}

func TestOperationMembershipRevisionTracksMembershipButNotCounterCheckpoints(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	startedAt := time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC)
	assert.Equal(int64(0), operationMembershipRevision(t, st))

	id := beginOperationHistoryInvocation(t, st, operations.KindMessageEmbedding, "revision:invocation", startedAt)
	assert.Equal(int64(1), operationMembershipRevision(t, st), "visible insert advances revision")
	require.NoError(st.CheckpointOperationInvocation(t.Context(), id,
		operations.InvocationCounters{Attempted: 1}))
	assert.Equal(int64(1), operationMembershipRevision(t, st), "counter checkpoints do not change membership")

	movedStart := startedAt.Add(time.Second)
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`
		UPDATE message_embedding_runs SET started_at = ? WHERE id = ?`), movedStart, operationInt64ID(t, id))
	require.NoError(err)
	assert.Equal(int64(2), operationMembershipRevision(t, st), "ordering changes advance revision")

	finishOperationHistoryInvocation(t, st, id, movedStart.Add(time.Minute), "succeeded", 1, 1, 0, "")
	assert.Equal(int64(3), operationMembershipRevision(t, st), "state-filter transitions advance revision")
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM message_embedding_runs WHERE id = ?`), operationInt64ID(t, id))
	require.NoError(err)
	assert.Equal(int64(4), operationMembershipRevision(t, st), "visible deletion advances revision")

	queuedID := insertPersonEnrichmentHistoryRun(t, st, personEnrichmentHistorySeed{
		requestedAt: startedAt,
		state:       "queued",
	})
	assert.Equal(int64(4), operationMembershipRevision(t, st), "unclaimed enrichment is not a history member")
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		UPDATE person_enrichment_runs SET started_at = ? WHERE id = ?`), startedAt, queuedID)
	require.NoError(err)
	assert.Equal(int64(5), operationMembershipRevision(t, st), "first enrichment claim adds history membership")
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		UPDATE person_enrichment_runs SET requested_count = 1 WHERE id = ?`), queuedID)
	require.NoError(err)
	assert.Equal(int64(5), operationMembershipRevision(t, st), "native counter updates do not change membership")
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		UPDATE person_enrichment_runs SET state = 'running' WHERE id = ?`), queuedID)
	require.NoError(err)
	assert.Equal(int64(6), operationMembershipRevision(t, st))
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM person_enrichment_runs WHERE id = ?`), queuedID)
	require.NoError(err)
	assert.Equal(int64(7), operationMembershipRevision(t, st))
}

func seedAllOperationHistoryKinds(t *testing.T, st *store.Store, startedAt time.Time) {
	t.Helper()
	seedAllOperationHistoryKindsAt(t, st, startedAt, "single")
}

func seedAllOperationHistoryKindsAt(
	t *testing.T, st *store.Store, startedAt time.Time, label string,
) []string {
	t.Helper()
	keys := make([]string, 0, len(allOperationHistoryKinds))

	cardDAVID := insertCardDAVOperationRun(t, st, cardDAVOperationRunSeed{
		startedAt: startedAt, trigger: "manual", state: "succeeded", books: 1,
	})
	keys = append(keys, "carddav_sync:"+fmtInt64(cardDAVID))

	for _, kind := range []operations.Kind{
		operations.KindDocumentEmbedding,
		operations.KindDocumentExtraction,
		operations.KindMessageEmbedding,
		operations.KindPersonEmbedding,
	} {
		id := beginOperationHistoryInvocation(t, st, kind, "mixed:"+label+":"+string(kind), startedAt)
		keys = append(keys, operationStableIDKey(t, id))
	}

	enrichmentID := insertPersonEnrichmentHistoryRun(t, st, personEnrichmentHistorySeed{
		requestedAt: startedAt.Add(-time.Minute),
		startedAt:   &startedAt,
		state:       "running",
	})
	keys = append(keys, "person_enrichment:"+fmtInt64(enrichmentID))

	insertPersonSweepOperationRun(t, st, personSweepOperationSeed{
		id: "mixed-person-sweep-" + label, trigger: "manual", state: "succeeded", startedAt: startedAt,
	})
	keys = append(keys, "person_sweep:mixed-person-sweep-"+label)

	source := createOperationSource(t, st, "mixed-history-"+label+"@example.invalid")
	sourceID := insertSourceOperationRun(t, st, source.ID, sourceOperationRunSeed{
		startedAt: startedAt, state: "completed", processed: 1,
	})
	keys = append(keys, "source_sync:"+fmtInt64(sourceID))

	visualID := beginOperationHistoryInvocation(t, st, operations.KindVisualEmbedding,
		"mixed:"+label+":visual_embedding", startedAt)
	keys = append(keys, operationStableIDKey(t, visualID))
	return keys
}

func beginOperationHistoryInvocation(
	t *testing.T, st *store.Store, kind operations.Kind, key string, startedAt time.Time,
) operations.StableID {
	t.Helper()
	result, err := st.BeginOperationInvocation(t.Context(), operations.InvocationSpec{
		Kind: kind, Key: key, Trigger: operations.TriggerManual, StartedAt: startedAt,
	})
	require.NoError(t, err)
	return result.ID
}

func finishOperationHistoryInvocation(
	t *testing.T,
	st *store.Store,
	id operations.StableID,
	finishedAt time.Time,
	state string,
	attempted, succeeded, failed int64,
	errorCode string,
) {
	t.Helper()
	ledger := map[operations.Kind]string{
		operations.KindMessageEmbedding:   "message_embedding_runs",
		operations.KindPersonEmbedding:    "person_embedding_runs",
		operations.KindDocumentExtraction: "document_extraction_runs",
		operations.KindDocumentEmbedding:  "document_embedding_runs",
		operations.KindVisualEmbedding:    "visual_embedding_runs",
	}[id.Kind()]
	require.NotEmpty(t, ledger)
	var storedError any
	if errorCode != "" {
		storedError = errorCode
	}
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE `+ledger+`
		SET state = ?, finished_at = ?, error_code = ?, attempted = ?, succeeded = ?, failed = ?
		WHERE id = ?`), state, finishedAt, storedError, attempted, succeeded, failed, operationInt64ID(t, id))
	require.NoError(t, err)
}

type personEnrichmentHistorySeed struct {
	requestedAt time.Time
	startedAt   *time.Time
	completedAt *time.Time
	state       string
	requested   int64
	started     int64
	succeeded   int64
	failed      int64
	suppressed  int64
	rejected    int64
	failure     string
}

func insertPersonEnrichmentHistoryRun(
	t *testing.T, st *store.Store, seed personEnrichmentHistorySeed,
) int64 {
	t.Helper()
	var id int64
	var startedAt, completedAt any
	if seed.startedAt != nil {
		startedAt = *seed.startedAt
	}
	if seed.completedAt != nil {
		completedAt = *seed.completedAt
	}
	var failure any
	if seed.failure != "" {
		failure = seed.failure
	}
	err := st.DB().QueryRowContext(t.Context(), st.Rebind(`
		INSERT INTO person_enrichment_runs
			(kind, requested_by, requested_at, started_at, completed_at, state,
			 requested_count, started_count, succeeded_count, failed_count,
			 suppressed_count, identity_rejected_count, failure_class)
		VALUES ('manual', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id`), "history:"+seed.requestedAt.Format(time.RFC3339Nano), seed.requestedAt,
		startedAt, completedAt, seed.state, seed.requested, seed.started, seed.succeeded,
		seed.failed, seed.suppressed, seed.rejected, failure).Scan(&id)
	require.NoError(t, err)
	return id
}

func operationMembershipRevision(t *testing.T, st *store.Store) int64 {
	t.Helper()
	var revision int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `
		SELECT membership_revision FROM operation_history_state WHERE singleton = 1`).Scan(&revision))
	return revision
}

func operationInt64ID(t *testing.T, id operations.StableID) int64 {
	t.Helper()
	value, ok := id.Int64()
	require.True(t, ok)
	return value
}

func operationStableIDKey(t *testing.T, id operations.StableID) string {
	t.Helper()
	if value, ok := id.Int64(); ok {
		return string(id.Kind()) + ":" + fmtInt64(value)
	}
	value, ok := id.Text()
	require.True(t, ok)
	return string(id.Kind()) + ":" + value
}
