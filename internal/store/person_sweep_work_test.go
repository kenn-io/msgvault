package store_test

import (
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
)

const (
	testSweepProgramFingerprint = "program-v1"
	testSweepCatalogFingerprint = "catalog-v1"
)

func reconcilePersonSweepFixture(
	t *testing.T, f personSweepJournalFixture, lanes ...peoplesweep.SourceClass,
) peoplesweep.GapResult {
	t.Helper()
	result, err := f.store.ReconcilePersonSweepWorkContext(t.Context(), peoplesweep.GapRequest{
		ProgramFingerprint: testSweepProgramFingerprint,
		CatalogFingerprint: testSweepCatalogFingerprint,
		SourceLanes:        lanes,
		Limit:              100,
		Now:                time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC),
		BackstopInterval:   24 * time.Hour,
	})
	require.NoError(t, err)
	return result
}

func claimPersonSweepFixture(t *testing.T, st *store.Store, worker string) *peoplesweep.Lease {
	t.Helper()
	lease, err := st.ClaimPersonSweep(t.Context(), peoplesweep.ClaimRequest{
		WorkerID: worker, LeaseDuration: time.Minute,
	})
	require.NoError(t, err)
	require.NotNil(t, lease)
	return lease
}

func personSweepConversationCursorKey(personID int64) peoplesweep.CursorKey {
	return peoplesweep.CursorKey{
		PersonID: personID, SourceLane: peoplesweep.SourceConversationText,
		ProgramFingerprint: testSweepProgramFingerprint,
		CatalogFingerprint: testSweepCatalogFingerprint,
	}
}

func loadPersonSweepCursor(
	t *testing.T, st *store.Store, key peoplesweep.CursorKey,
) peoplesweep.Cursor {
	t.Helper()
	var (
		cursor       peoplesweep.Cursor
		lastBackstop sql.NullTime
	)
	cursor.Key = key
	err := st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT optimistic_sequence, optimistic_document_key,
		       reconcile_upper_key, reconcile_after_key, reconcile_document_key,
		       reconciliation_complete, backstop_document_key, last_backstop_at
		FROM person_sweep_cursors
		WHERE person_id = ? AND source_lane = ? AND program_fingerprint = ?
		  AND catalog_fingerprint = ?`), key.PersonID, key.SourceLane,
		key.ProgramFingerprint, key.CatalogFingerprint).Scan(
		&cursor.OptimisticSequence, &cursor.OptimisticDocumentKey, &cursor.ReconcileUpperKey,
		&cursor.ReconcileAfterKey, &cursor.ReconcileDocumentKey, &cursor.ReconciliationComplete,
		&cursor.BackstopDocumentKey, &lastBackstop)
	require.NoError(t, err)
	if lastBackstop.Valid {
		value := lastBackstop.Time.UTC()
		cursor.LastBackstopAt = &value
	}
	return cursor
}

func TestPersonSweepExpiredLeaseReclaimsWithNewFence(t *testing.T) {
	f := newPersonSweepJournalFixture(t, true, false)
	f.insertMessage(t, "expired-lease", "email", f.aliceID,
		time.Date(2026, 8, 23, 11, 0, 0, 0, time.UTC))
	reconcilePersonSweepFixture(t, f, peoplesweep.SourceConversationText)

	first := claimPersonSweepFixture(t, f.store, "worker-a")
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		UPDATE person_sweep_work SET lease_until = ? WHERE person_id = ?`),
		time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), first.PersonID)
	require.NoError(t, err)

	second := claimPersonSweepFixture(t, f.store, "worker-b")
	assert.Equal(t, first.PersonID, second.PersonID)
	assert.Equal(t, first.Fence+1, second.Fence)
}

func TestEnsurePersonSweepCursorsCompletesAndRepairsEmptyLane(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	key := peoplesweep.CursorKey{PersonID: f.alicePersonID,
		SourceLane:         peoplesweep.SourceAttachmentOCR,
		ProgramFingerprint: testSweepProgramFingerprint,
		CatalogFingerprint: testSweepCatalogFingerprint,
	}
	cursors, err := f.store.EnsurePersonSweepCursors(t.Context(), []peoplesweep.CursorKey{key})
	must.NoError(err)
	must.Len(cursors, 1)
	checks.True(cursors[0].ReconciliationComplete)

	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		UPDATE person_sweep_cursors SET reconciliation_complete = FALSE
		WHERE person_id = ? AND source_lane = ? AND program_fingerprint = ?
		  AND catalog_fingerprint = ?`), key.PersonID, key.SourceLane,
		key.ProgramFingerprint, key.CatalogFingerprint)
	must.NoError(err)
	cursors, err = f.store.EnsurePersonSweepCursors(t.Context(), []peoplesweep.CursorKey{key})
	must.NoError(err)
	must.Len(cursors, 1)
	checks.True(cursors[0].ReconciliationComplete)
}

func TestPersonSweepExpiredLeaseReclaimFinalizesAbandonedAccounting(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	f.insertMessage(t, "expired-accounting", "email", f.aliceID,
		time.Date(2026, 8, 23, 11, 0, 0, 0, time.UTC))
	reconcilePersonSweepFixture(t, f, peoplesweep.SourceConversationText)
	first := claimPersonSweepFixture(t, f.store, "worker-a")
	fixture := personSweepBudgetFixture{store: f.store, personID: first.PersonID,
		runID: "run-abandoned", attemptID: "attempt-abandoned"}
	_, err := f.store.StartPersonSweepRun(t.Context(), peoplesweep.StartRun{
		ID: fixture.runID, Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental,
		ProgramFingerprint: "program-fingerprint", CatalogFingerprint: "catalog-fingerprint",
		ProviderFingerprint: "provider-fingerprint", StartedAt: sweepBudgetNow(),
	})
	must.NoError(err)
	must.NoError(f.store.StartPersonSweepAttempt(t.Context(),
		sweepStartAttempt(t, fixture.attemptID, fixture.runID, fixture.personID, first.Fence)))
	reservation, err := f.store.ReservePersonSweepBudget(t.Context(),
		sweepReservation(fixture, 0, 100, "provider-fingerprint", generousSweepBudget()))
	must.NoError(err)
	must.NoError(f.store.MarkPersonSweepBudgetStarted(t.Context(), reservation))
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`UPDATE person_sweep_work SET lease_until = ? WHERE person_id = ?`),
		time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), first.PersonID)
	must.NoError(err)

	second := claimPersonSweepFixture(t, f.store, "worker-b")
	checks.Equal(first.Fence+1, second.Fence)
	var attemptStatus, failureClass, batchStatus, runStatus string
	must.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(
		`SELECT status, failure_class FROM person_sweep_attempts WHERE id = ?`),
		fixture.attemptID).Scan(&attemptStatus, &failureClass))
	must.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(
		`SELECT status FROM person_sweep_batches WHERE attempt_id = ? AND batch_ordinal = 0`),
		fixture.attemptID).Scan(&batchStatus))
	must.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(
		`SELECT status FROM person_sweep_runs WHERE id = ?`), fixture.runID).Scan(&runStatus))
	checks.Equal("failed", attemptStatus)
	checks.Equal(string(peoplesweep.FailureLeaseLost), failureClass)
	checks.Equal("failed", batchStatus)
	checks.Equal("failed", runStatus)
	var reservedRequests, actualRequests int64
	must.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(
		`SELECT reserved_requests, actual_requests FROM person_sweep_daily_usage WHERE utc_day = ?`),
		testSweepUTCDate).Scan(&reservedRequests, &actualRequests))
	checks.Zero(reservedRequests)
	checks.Equal(int64(1), actualRequests,
		"a request marked started remains conservatively charged after worker loss")
}

func TestPersonSweepConcurrentClaimHasOneWinner(t *testing.T) {
	f := newPersonSweepJournalFixture(t, true, false)
	f.insertMessage(t, "concurrent-claim", "email", f.aliceID,
		time.Date(2026, 8, 23, 11, 0, 0, 0, time.UTC))
	reconcilePersonSweepFixture(t, f, peoplesweep.SourceConversationText)

	start := make(chan struct{})
	results := make(chan *peoplesweep.Lease, 2)
	errs := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for _, worker := range []string{"worker-a", "worker-b"} {
		go func(worker string) {
			ready.Done()
			<-start
			lease, err := f.store.ClaimPersonSweep(t.Context(), peoplesweep.ClaimRequest{
				WorkerID: worker, LeaseDuration: time.Minute,
			})
			results <- lease
			errs <- err
		}(worker)
	}
	ready.Wait()
	close(start)

	var winners []*peoplesweep.Lease
	for range 2 {
		require.NoError(t, <-errs)
		if lease := <-results; lease != nil {
			winners = append(winners, lease)
		}
	}
	require.Len(t, winners, 1)
	assert.Equal(t, f.alicePersonID, winners[0].PersonID)
}

func TestPersonSweepLeaseRenewalRequiresExactFence(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	f.insertMessage(t, "renew-lease", "email", f.aliceID,
		time.Date(2026, 8, 23, 11, 0, 0, 0, time.UTC))
	reconcilePersonSweepFixture(t, f, peoplesweep.SourceConversationText)
	lease := claimPersonSweepFixture(t, f.store, "worker-a")

	renewed, err := f.store.RenewPersonSweep(t.Context(), *lease, 2*time.Minute)
	requirements.NoError(err)
	requirements.NotNil(renewed)
	checks.Equal(lease.Fence, renewed.Fence)
	checks.Greater(renewed.ExpiresAt, lease.ExpiresAt)

	stale := *renewed
	stale.Fence--
	_, err = f.store.RenewPersonSweep(t.Context(), stale, time.Minute)
	requirements.ErrorIs(err, peoplesweep.ErrLeaseLost)
}

func TestPersonSweepRetryAvailabilityUsesDatabaseTime(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	f.insertMessage(t, "retry-database-time", "email", f.aliceID,
		time.Date(2026, 8, 23, 11, 0, 0, 0, time.UTC))
	reconcilePersonSweepFixture(t, f, peoplesweep.SourceConversationText)
	lease := claimPersonSweepFixture(t, f.store, "worker-a")
	retryAt := time.Now().UTC().Add(time.Hour)
	requirements.NoError(f.store.FailPersonSweepWork(t.Context(), peoplesweep.WorkFailure{
		Lease: *lease, AttemptID: "attempt-1", Class: peoplesweep.FailureTimeout,
		RetryAt: retryAt,
	}))

	notYet, err := f.store.ClaimPersonSweep(t.Context(), peoplesweep.ClaimRequest{
		WorkerID: "worker-b", LeaseDuration: time.Minute,
		AvailableAt: retryAt.Add(time.Hour),
	})
	requirements.NoError(err)
	checks.Nil(notYet, "caller time must not make a database-future retry available")

	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		UPDATE person_sweep_work SET available_at = CURRENT_TIMESTAMP WHERE person_id = ?`),
		f.alicePersonID)
	requirements.NoError(err)
	checks.NotNil(claimPersonSweepFixture(t, f.store, "worker-b"))
}

func TestPersonSweepForcedBackstopMakesFutureRetryClaimable(t *testing.T) {
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	now := time.Now().UTC()
	key := peoplesweep.CursorKey{PersonID: f.alicePersonID,
		SourceLane:         peoplesweep.SourceConversationText,
		ProgramFingerprint: "program-force-backstop",
		CatalogFingerprint: "catalog-force-backstop"}
	_, err := f.store.EnsurePersonSweepCursors(t.Context(), []peoplesweep.CursorKey{key})
	requirements.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		UPDATE person_sweep_cursors SET last_backstop_at = ?
		WHERE person_id = ? AND source_lane = ? AND program_fingerprint = ?
		  AND catalog_fingerprint = ?`), now, f.alicePersonID, key.SourceLane,
		key.ProgramFingerprint, key.CatalogFingerprint)
	requirements.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		UPDATE person_sweep_work SET available_at = ?, lease_owner = '', lease_until = NULL
		WHERE person_id = ?`), now.Add(time.Hour), f.alicePersonID)
	requirements.NoError(err)

	_, err = f.store.ReconcilePersonSweepWorkContext(t.Context(), peoplesweep.GapRequest{
		ProgramFingerprint: key.ProgramFingerprint, CatalogFingerprint: key.CatalogFingerprint,
		SourceLanes: []peoplesweep.SourceClass{key.SourceLane}, Limit: 10, Now: now,
		BackstopInterval: 24 * time.Hour, ForceBackstop: true,
	})
	requirements.NoError(err)
	lease, err := f.store.ClaimPersonSweep(t.Context(), peoplesweep.ClaimRequest{
		WorkerID: "forced-backstop-worker", LeaseDuration: time.Minute})
	requirements.NoError(err)
	requirements.NotNil(lease)
	assert.Equal(t, f.alicePersonID, lease.PersonID)
}

func TestPersonSweepWorkCoalescesDirtyHighWater(t *testing.T) {
	checks := assert.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	for i := range 3 {
		f.insertMessage(t, fmt.Sprintf("coalesce-%d", i), "email", f.aliceID,
			time.Date(2026, 8, 23, 11, i, 0, 0, time.UTC))
	}
	want := latestPersonSweepSequence(t, f.store)
	result := reconcilePersonSweepFixture(t, f, peoplesweep.SourceConversationText)

	var (
		rows int
		got  int64
	)
	err := f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*), MAX(dirty_through_sequence)
		FROM person_sweep_work WHERE person_id = ?`), f.alicePersonID).Scan(&rows, &got)
	require.NoError(t, err)
	checks.Equal(1, rows)
	checks.Equal(want, got)
	checks.Equal(1, result.WorkCreated)
}

func TestPersonSweepCursorCASRejectsStaleSequence(t *testing.T) {
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	f.insertMessage(t, "cursor-cas", "email", f.aliceID,
		time.Date(2026, 8, 23, 11, 0, 0, 0, time.UTC))
	key := personSweepConversationCursorKey(f.alicePersonID)
	cursors, err := f.store.EnsurePersonSweepCursors(t.Context(), []peoplesweep.CursorKey{key})
	requirements.NoError(err)
	requirements.Len(cursors, 1)
	reconcilePersonSweepFixture(t, f, peoplesweep.SourceConversationText)
	lease := claimPersonSweepFixture(t, f.store, "worker-a")

	err = f.store.AdvancePersonSweepReconciliation(t.Context(), *lease, peoplesweep.GenerationCursor{
		Key: key, Mode: peoplesweep.GenerationCursorOptimistic,
		CursorFrom:    cursors[0].OptimisticSequence + 1,
		CursorThrough: cursors[0].OptimisticSequence + 10,
	})
	requirements.NoError(err)
	assert.Equal(t, cursors[0], loadPersonSweepCursor(t, f.store, key))
}

func TestPersonSweepCursorPersistsPartialDocumentProgress(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	f.insertMessage(t, "document-cursor", "email", f.aliceID,
		time.Date(2026, 8, 23, 11, 0, 0, 0, time.UTC))
	key := personSweepConversationCursorKey(f.alicePersonID)
	key.SourceLane = peoplesweep.SourceDocumentText
	cursors, err := f.store.EnsurePersonSweepCursors(t.Context(), []peoplesweep.CursorKey{key})
	must.NoError(err)
	must.Len(cursors, 1)
	reconcilePersonSweepFixture(t, f, peoplesweep.SourceDocumentText)
	lease := claimPersonSweepFixture(t, f.store, "worker-a")

	err = f.store.AdvancePersonSweepReconciliation(t.Context(), *lease, peoplesweep.GenerationCursor{
		Key: key, Mode: peoplesweep.GenerationCursorOptimistic,
		CursorFrom: cursors[0].OptimisticSequence, CursorThrough: cursors[0].OptimisticSequence,
		DocumentToKey: "live:00000000000000000002",
	})
	must.NoError(err)
	got := loadPersonSweepCursor(t, f.store, key)
	checks.Equal(cursors[0].OptimisticSequence, got.OptimisticSequence)
	checks.Equal("live:00000000000000000002", got.OptimisticDocumentKey)
}

func TestPersonSweepCursorSeparatesSourceAndFingerprints(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	f.insertMessage(t, "cursor-lanes", "email", f.aliceID,
		time.Date(2026, 8, 23, 11, 0, 0, 0, time.UTC))
	base := personSweepConversationCursorKey(f.alicePersonID)
	meeting := base
	meeting.SourceLane = peoplesweep.SourceMeetingText
	program := base
	program.ProgramFingerprint = "program-v2"
	catalog := base
	catalog.CatalogFingerprint = "catalog-v2"

	cursors, err := f.store.EnsurePersonSweepCursors(t.Context(), []peoplesweep.CursorKey{
		base, meeting, program, catalog,
	})
	requirements.NoError(err)
	requirements.Len(cursors, 4)
	reconcilePersonSweepFixture(t, f, peoplesweep.SourceConversationText)
	lease := claimPersonSweepFixture(t, f.store, "worker-a")

	err = f.store.AdvancePersonSweepReconciliation(t.Context(), *lease, peoplesweep.GenerationCursor{
		Key: base, Mode: peoplesweep.GenerationCursorOptimistic,
		CursorFrom:    cursors[0].OptimisticSequence,
		CursorThrough: cursors[0].OptimisticSequence + 1,
	})
	requirements.NoError(err)
	checks.Equal(cursors[0].OptimisticSequence+1,
		loadPersonSweepCursor(t, f.store, base).OptimisticSequence)
	checks.Equal(cursors[1], loadPersonSweepCursor(t, f.store, meeting))
	checks.Equal(cursors[2], loadPersonSweepCursor(t, f.store, program))
	checks.Equal(cursors[3], loadPersonSweepCursor(t, f.store, catalog))
}

func TestPersonSweepReconciliationStopsAtCapturedUpperKey(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	firstMessage := f.insertMessage(t, "captured-upper", "email", f.aliceID,
		time.Date(2026, 8, 23, 11, 0, 0, 0, time.UTC))
	key := personSweepConversationCursorKey(f.alicePersonID)
	cursors, err := f.store.EnsurePersonSweepCursors(t.Context(), []peoplesweep.CursorKey{key})
	requirements.NoError(err)
	requirements.Len(cursors, 1)
	captured := cursors[0]
	checks.Equal(fmt.Sprintf("%020d", firstMessage), captured.ReconcileUpperKey)

	secondMessage := f.insertMessage(t, "above-upper", "email", f.aliceID,
		time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
	liveSequence := latestPersonSweepSequence(t, f.store)
	reconcilePersonSweepFixture(t, f, peoplesweep.SourceConversationText)
	lease := claimPersonSweepFixture(t, f.store, "worker-a")
	requirements.NoError(f.store.AdvancePersonSweepReconciliation(
		t.Context(), *lease, peoplesweep.GenerationCursor{
			Key: key, Mode: peoplesweep.GenerationCursorReconciliation,
			ReconcileFromKey: "", ReconcileToKey: captured.ReconcileUpperKey,
		}))

	got := loadPersonSweepCursor(t, f.store, key)
	checks.Equal(captured.ReconcileUpperKey, got.ReconcileUpperKey)
	checks.Equal(captured.ReconcileUpperKey, got.ReconcileAfterKey)
	checks.True(got.ReconciliationComplete)
	checks.Equal(captured.OptimisticSequence, got.OptimisticSequence)
	checks.Greater(fmt.Sprintf("%020d", secondMessage), got.ReconcileUpperKey)
	var dirtyThrough int64
	err = f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT dirty_through_sequence FROM person_sweep_work WHERE person_id = ?`),
		f.alicePersonID).Scan(&dirtyThrough)
	requirements.NoError(err)
	checks.Equal(liveSequence, dirtyThrough)
	checks.Greater(dirtyThrough, got.OptimisticSequence)
}

func TestPersonSweepCursorMutationRequiresLeasePerson(t *testing.T) {
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, true)
	f.insertMessage(t, "lease-person-alice", "email", f.aliceID,
		time.Date(2026, 8, 23, 11, 0, 0, 0, time.UTC))
	f.insertMessage(t, "lease-person-bob", "email", f.bobID,
		time.Date(2026, 8, 23, 11, 1, 0, 0, time.UTC))
	aliceKey := personSweepConversationCursorKey(f.alicePersonID)
	bobKey := personSweepConversationCursorKey(f.bobPersonID)
	cursors, err := f.store.EnsurePersonSweepCursors(
		t.Context(), []peoplesweep.CursorKey{aliceKey, bobKey})
	requirements.NoError(err)
	requirements.Len(cursors, 2)
	reconcilePersonSweepFixture(t, f, peoplesweep.SourceConversationText)
	aliceLease := claimPersonSweepFixture(t, f.store, "shared-worker")
	bobLease := claimPersonSweepFixture(t, f.store, "shared-worker")
	requirements.Equal(f.alicePersonID, aliceLease.PersonID)
	requirements.Equal(f.bobPersonID, bobLease.PersonID)
	requirements.Equal(aliceLease.Fence, bobLease.Fence,
		"the regression requires equal fence numbers on separate work rows")

	err = f.store.AdvancePersonSweepReconciliation(t.Context(), *aliceLease,
		peoplesweep.GenerationCursor{
			Key: bobKey, Mode: peoplesweep.GenerationCursorOptimistic,
			CursorFrom:    cursors[1].OptimisticSequence,
			CursorThrough: cursors[1].OptimisticSequence + 1,
		})
	requirements.ErrorIs(err, peoplesweep.ErrLeaseLost)
	assert.Equal(t, cursors[1], loadPersonSweepCursor(t, f.store, bobKey))
}
