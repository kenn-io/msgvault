package document

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestReconcilerRunActivatesCompleteBuildingGeneration(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	worker := &fakeDocumentVectorWorkerRunner{result: RunResult{Claimed: 1, Embedded: 1, Published: 1}}
	ledger := newFakeDocumentVectorReconcileLedger(store.DocumentVectorGenerationBuilding)
	ledger.coverage = store.DocumentVectorCoverage{Required: 1, Ready: 1}
	backend := &fakeDocumentVectorBackend{}
	reconciler := NewReconciler(ReconcilerDeps{
		Ledger: ledger, Worker: worker, Backend: backend,
		Now: func() time.Time { return workerNow },
	})

	result, err := reconciler.Run(t.Context(), 1, 10)
	requirements.NoError(err)
	assertions.True(result.WorkerRan)
	assertions.Equal(worker.result, result.Worker)
	assertions.Equal(1, worker.calls)
	assertions.Equal([]int{10}, worker.limits)
	assertions.Equal(ledger.coverage, result.Coverage)
	assertions.True(result.Activated)
	assertions.True(result.Converged)
	assertions.Equal(1, ledger.activateCalls)
	assertions.Zero(ledger.purgeCalls)
	assertions.Empty(backend.deletes)
}

func TestReconcilerRunRejectsInvalidRequestAndMissingBuildingWorker(t *testing.T) {
	requirements := require.New(t)
	ledger := newFakeDocumentVectorReconcileLedger(store.DocumentVectorGenerationBuilding)
	reconciler := NewReconciler(ReconcilerDeps{
		Ledger: ledger, Backend: &fakeDocumentVectorBackend{}, Now: func() time.Time { return workerNow },
	})

	_, err := reconciler.Run(t.Context(), 0, 1)
	requirements.ErrorContains(err, "generation")
	_, err = reconciler.Run(t.Context(), 1, 0)
	requirements.ErrorContains(err, "limit")
	_, err = reconciler.Run(t.Context(), 1, 1001)
	requirements.ErrorContains(err, "limit")
	_, err = reconciler.Run(t.Context(), 1, 1)
	requirements.ErrorContains(err, "worker")
}

func TestReconcilerRunTimestampsCleanupAndActivationAfterWorker(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	worker := &fakeDocumentVectorWorkerRunner{}
	ledger := newFakeDocumentVectorReconcileLedger(store.DocumentVectorGenerationBuilding)
	ledger.coverage = store.DocumentVectorCoverage{Required: 1, Ready: 1}
	ledger.obsolete = []store.DocumentVectorCleanupToken{{GenerationID: 1, Token: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"}}
	ledger.status.CleanupPending = 1
	base := workerNow.Add(123456 * time.Nanosecond)
	clockCalls := 0
	reconciler := NewReconciler(ReconcilerDeps{
		Ledger: ledger, Worker: worker, Backend: &fakeDocumentVectorBackend{},
		Now: func() time.Time {
			at := base.Add(time.Duration(clockCalls) * time.Second)
			clockCalls++
			return at
		},
	})

	_, err := reconciler.Run(t.Context(), 1, 10)
	requirements.NoError(err)
	assertions.Equal(base.UTC().Truncate(time.Millisecond), ledger.parkAt)
	assertions.Equal(base.Add(time.Second).UTC().Truncate(time.Millisecond), ledger.finalizeAt)
	assertions.Equal(base.Add(2*time.Second).UTC().Truncate(time.Millisecond), ledger.activateAt)
}

func TestReconcilerRunContinuesSafeWorkAfterWorkerErrorWithoutActivation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	workerErr := errors.New("provider unavailable")
	worker := &fakeDocumentVectorWorkerRunner{err: workerErr}
	ledger := newFakeDocumentVectorReconcileLedger(store.DocumentVectorGenerationBuilding)
	ledger.coverage = store.DocumentVectorCoverage{Required: 1, Ready: 1}
	ledger.obsolete = []store.DocumentVectorCleanupToken{{GenerationID: 1, Token: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	ledger.status.CleanupPending = 1
	backend := &fakeDocumentVectorBackend{}
	reconciler := NewReconciler(ReconcilerDeps{Ledger: ledger, Worker: worker, Backend: backend, Now: func() time.Time { return workerNow }})

	result, err := reconciler.Run(t.Context(), 1, 10)
	requirements.ErrorIs(err, workerErr)
	assertions.Equal(1, worker.calls)
	assertions.Equal(1, result.CleanupDeleted)
	assertions.Equal(1, result.CleanupFinalized)
	assertions.Equal(1, ledger.statusCalls)
	assertions.Equal(1, ledger.coverageCalls)
	assertions.Zero(ledger.activateCalls)
	assertions.False(result.Activated)
	assertions.False(result.Converged)
	assertions.Equal([][]string{{ledger.obsolete[0].Token}}, backend.deletes)
}

func TestReconcilerRunNeverWorksActiveGeneration(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	ledger := newFakeDocumentVectorReconcileLedger(store.DocumentVectorGenerationActive)
	ledger.coverage = store.DocumentVectorCoverage{Required: 2, Ready: 2}
	reconciler := NewReconciler(ReconcilerDeps{
		Ledger: ledger, Backend: &fakeDocumentVectorBackend{},
		Now: func() time.Time { return workerNow },
	})

	result, err := reconciler.Run(t.Context(), 1, 10)
	requirements.NoError(err)
	assertions.False(result.WorkerRan)
	assertions.True(result.Converged)
	assertions.Zero(ledger.activateCalls)
	assertions.Zero(ledger.purgeCalls)
}

func TestReconcilerRunCleansRetiredGenerationWithoutWorkerDependency(t *testing.T) {
	ledger := newFakeDocumentVectorReconcileLedger(store.DocumentVectorGenerationRetired)
	ledger.purgeResult = true
	reconciler := NewReconciler(ReconcilerDeps{
		Ledger: ledger, Backend: &fakeDocumentVectorBackend{}, Now: func() time.Time { return workerNow },
	})

	result, err := reconciler.Run(t.Context(), 1, 10)
	require.NoError(t, err)
	assert.False(t, result.WorkerRan)
	assert.True(t, result.Purged)
}

func TestReconcilerRunReplaysRetiredDeleteAfterFinalizeCrashThenPurges(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	finalizeErr := errors.New("main store unavailable")
	worker := &fakeDocumentVectorWorkerRunner{}
	ledger := newFakeDocumentVectorReconcileLedger(store.DocumentVectorGenerationRetired)
	ledger.obsolete = []store.DocumentVectorCleanupToken{{GenerationID: 1, Token: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}
	ledger.status.CleanupPending = 1
	ledger.finalizeErr = finalizeErr
	ledger.purgeResult = true
	backend := &fakeDocumentVectorBackend{}
	reconciler := NewReconciler(ReconcilerDeps{Ledger: ledger, Worker: worker, Backend: backend, Now: func() time.Time { return workerNow }})

	first, err := reconciler.Run(t.Context(), 1, 10)
	requirements.ErrorIs(err, finalizeErr)
	assertions.Zero(worker.calls)
	assertions.Equal(1, first.CleanupDeleted)
	assertions.Zero(first.CleanupFinalized)
	assertions.Zero(first.CleanupAfterGenerationID)
	assertions.Empty(first.CleanupAfterToken)
	assertions.Zero(ledger.purgeCalls)

	ledger.finalizeErr = nil
	second, err := reconciler.Run(t.Context(), 1, 10)
	requirements.NoError(err)
	assertions.Equal(1, second.CleanupDeleted)
	assertions.Equal(1, second.CleanupFinalized)
	assertions.True(second.Purged)
	assertions.True(second.Converged)
	assertions.Equal(1, ledger.purgeCalls)
	assertions.Equal([][]string{{ledger.obsolete[0].Token}, {ledger.obsolete[0].Token}}, backend.deletes)
}

func TestReconcilerRunReplaysParkedPageAfterBackendDeleteFailure(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	deleteErr := errors.New("backend unavailable")
	worker := &fakeDocumentVectorWorkerRunner{}
	ledger := newFakeDocumentVectorReconcileLedger(store.DocumentVectorGenerationBuilding)
	ledger.obsolete = []store.DocumentVectorCleanupToken{{GenerationID: 1, Token: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}}
	ledger.status.CleanupPending = 1
	backend := &fakeDocumentVectorBackend{deleteErr: deleteErr}
	reconciler := NewReconciler(ReconcilerDeps{Ledger: ledger, Worker: worker, Backend: backend, Now: func() time.Time { return workerNow }})

	first, err := reconciler.Run(t.Context(), 1, 10)
	requirements.ErrorIs(err, deleteErr)
	assertions.Equal(1, first.CleanupListed)
	assertions.Zero(first.CleanupFinalized)
	assertions.Zero(first.CleanupAfterGenerationID)
	assertions.Empty(first.CleanupAfterToken)
	assertions.False(first.CleanupExhausted)

	backend.deleteErr = nil
	second, err := reconciler.Run(t.Context(), 1, 10)
	requirements.NoError(err)
	assertions.Equal(1, second.CleanupDeleted)
	assertions.Equal(1, second.CleanupFinalized)
	assertions.Equal([][]string{{ledger.obsolete[0].Token}, {ledger.obsolete[0].Token}}, backend.deletes)
}

func TestReconcilerRunDoesNotAdvancePastPartialFinalizeFailure(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	finalizeErr := errors.New("finalize interrupted")
	worker := &fakeDocumentVectorWorkerRunner{}
	ledger := newFakeDocumentVectorReconcileLedger(store.DocumentVectorGenerationBuilding)
	ledger.obsolete = []store.DocumentVectorCleanupToken{
		{GenerationID: 1, Token: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{GenerationID: 1, Token: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
	}
	ledger.status.CleanupPending = 2
	ledger.finalizeErrFor = ledger.obsolete[1].Token
	ledger.finalizeErr = finalizeErr
	backend := &fakeDocumentVectorBackend{}
	reconciler := NewReconciler(ReconcilerDeps{Ledger: ledger, Worker: worker, Backend: backend, Now: func() time.Time { return workerNow }})

	first, err := reconciler.Run(t.Context(), 1, 2)
	requirements.ErrorIs(err, finalizeErr)
	assertions.Equal(1, first.CleanupFinalized)
	assertions.Zero(first.CleanupAfterGenerationID)
	assertions.Empty(first.CleanupAfterToken)

	ledger.finalizeErr = nil
	second, err := reconciler.Run(t.Context(), 1, 2)
	requirements.NoError(err)
	assertions.Equal(1, second.CleanupListed)
	assertions.Equal(ledger.obsolete[1].Token, ledger.finalized[len(ledger.finalized)-1])
	assertions.Equal([][]string{
		{ledger.obsolete[0].Token, ledger.obsolete[1].Token},
		{ledger.obsolete[1].Token},
	}, backend.deletes)
}

func TestReconcilerRunBoundsCleanupAndReturnsRestorableCursor(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	worker := &fakeDocumentVectorWorkerRunner{}
	ledger := newFakeDocumentVectorReconcileLedger(store.DocumentVectorGenerationBuilding)
	ledger.coverage = store.DocumentVectorCoverage{Required: 1}
	ledger.obsolete = []store.DocumentVectorCleanupToken{
		{GenerationID: 1, Token: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{GenerationID: 1, Token: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
	}
	ledger.status.CleanupPending = 2
	backend := &fakeDocumentVectorBackend{}
	reconciler := NewReconciler(ReconcilerDeps{Ledger: ledger, Worker: worker, Backend: backend, Now: func() time.Time { return workerNow }})

	first, err := reconciler.Run(t.Context(), 1, 1)
	requirements.NoError(err)
	assertions.Equal(1, first.CleanupListed)
	assertions.Equal(GenerationID(1), first.CleanupAfterGenerationID)
	assertions.Equal(ledger.obsolete[0].Token, first.CleanupAfterToken)
	assertions.False(first.CleanupExhausted)

	second, err := reconciler.Run(t.Context(), 1, 1)
	requirements.NoError(err)
	assertions.Equal(1, second.CleanupListed)
	assertions.Zero(second.CleanupAfterGenerationID)
	assertions.Empty(second.CleanupAfterToken)
	assertions.True(second.CleanupExhausted)
	assertions.Equal([][]string{{ledger.obsolete[0].Token}, {ledger.obsolete[1].Token}}, backend.deletes)
}

type fakeDocumentVectorWorkerRunner struct {
	result RunResult
	err    error
	calls  int
	limits []int
}

func (w *fakeDocumentVectorWorkerRunner) Run(_ context.Context, _ GenerationID, limit int) (RunResult, error) {
	w.calls++
	w.limits = append(w.limits, limit)
	return w.result, w.err
}

type fakeDocumentVectorReconcileLedger struct {
	generation     store.DocumentVectorGeneration
	status         store.DocumentVectorGenerationStatus
	coverage       store.DocumentVectorCoverage
	obsolete       []store.DocumentVectorCleanupToken
	finalized      []string
	finalizeErr    error
	finalizeErrFor string
	activateErr    error
	purgeResult    bool
	purgeErr       error
	activateCalls  int
	purgeCalls     int
	statusCalls    int
	coverageCalls  int
	finalizedSet   map[string]bool
	parkAt         time.Time
	finalizeAt     time.Time
	activateAt     time.Time
}

func newFakeDocumentVectorReconcileLedger(state store.DocumentVectorGenerationState) *fakeDocumentVectorReconcileLedger {
	return &fakeDocumentVectorReconcileLedger{
		generation:   store.DocumentVectorGeneration{ID: 1, State: state},
		status:       store.DocumentVectorGenerationStatus{GenerationID: 1, State: state, FailuresExhausted: true},
		finalizedSet: map[string]bool{},
	}
}

func (l *fakeDocumentVectorReconcileLedger) GetDocumentVectorGeneration(context.Context, int64) (store.DocumentVectorGeneration, error) {
	return l.generation, nil
}

func (l *fakeDocumentVectorReconcileLedger) GetDocumentVectorGenerationStatus(context.Context, int64, string, int) (store.DocumentVectorGenerationStatus, error) {
	l.statusCalls++
	return l.status, nil
}

func (l *fakeDocumentVectorReconcileLedger) ParkObsoleteDocumentVectorTokens(_ context.Context, generationID int64, after string, limit int, now time.Time) (store.DocumentVectorCleanupPage, error) {
	l.parkAt = now
	page := store.DocumentVectorCleanupPage{}
	var tokens []store.DocumentVectorCleanupToken
	for _, token := range l.obsolete {
		if token.Token <= after || l.finalizedSet[token.Token] {
			continue
		}
		tokens = append(tokens, token)
		if len(tokens) == limit {
			break
		}
	}
	page.Tokens = tokens
	if len(tokens) < limit {
		page.Exhausted = true
	} else {
		page.AfterGenerationID = generationID
		page.AfterToken = tokens[len(tokens)-1].Token
	}
	return page, nil
}

func (l *fakeDocumentVectorReconcileLedger) FinalizeObsoleteDocumentVectorToken(_ context.Context, _ int64, token string, now time.Time) (bool, error) {
	l.finalizeAt = now
	if l.finalizeErr != nil && (l.finalizeErrFor == "" || l.finalizeErrFor == token) {
		return false, l.finalizeErr
	}
	l.finalized = append(l.finalized, token)
	l.finalizedSet[token] = true
	if l.status.CleanupPending > 0 {
		l.status.CleanupPending--
	}
	return true, nil
}

func (l *fakeDocumentVectorReconcileLedger) GetDocumentVectorCoverage(context.Context, int64) (store.DocumentVectorCoverage, error) {
	l.coverageCalls++
	return l.coverage, nil
}

func (l *fakeDocumentVectorReconcileLedger) ActivateDocumentVectorGeneration(_ context.Context, _ int64, now time.Time) error {
	l.activateCalls++
	l.activateAt = now
	if l.activateErr == nil {
		l.generation.State = store.DocumentVectorGenerationActive
		l.status.State = store.DocumentVectorGenerationActive
	}
	return l.activateErr
}

func (l *fakeDocumentVectorReconcileLedger) PurgeRetiredDocumentVectorGeneration(context.Context, int64) (bool, error) {
	l.purgeCalls++
	return l.purgeResult, l.purgeErr
}
