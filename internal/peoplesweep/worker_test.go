package peoplesweep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
)

func TestPersonFactSourceCursorsEncodeOptimisticRange(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	key := CursorKey{PersonID: 7, SourceLane: SourceConversationText,
		ProgramFingerprint: "program", CatalogFingerprint: "catalog"}
	got, hash, err := PersonFactSourceCursors([]GenerationCursor{{
		Key: key, Mode: GenerationCursorOptimistic, CursorFrom: 4, CursorThrough: 9,
	}})
	requirements.NoError(err)
	requirements.Len(got, 1)
	checks.Equal("person-sweep/v1/conversation_text/optimistic", got[0].Lane)
	// Exact compact bytes are part of the durable cursor contract.
	//nolint:testifylint
	checks.Equal(`{"bound":"exclusive","sequence":4}`, got[0].Start)
	//nolint:testifylint
	checks.Equal(`{"bound":"inclusive","sequence":9}`, got[0].End)
	checks.Len(hash, 64)
}

func TestPersonFactSourceCursorsEncodeReconciliationRange(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	key := CursorKey{PersonID: 7, SourceLane: SourceMeetingText,
		ProgramFingerprint: "program", CatalogFingerprint: "catalog"}
	got, _, err := PersonFactSourceCursors([]GenerationCursor{{
		Key: key, Mode: GenerationCursorReconciliation,
		ReconcileFromKey: "", ReconcileToKey: "\u03bb/\u96ea\n\"quoted\"",
	}})
	requirements.NoError(err)
	requirements.Len(got, 1)
	var start, end sourceKeyCursorCoordinate
	requirements.NoError(json.Unmarshal([]byte(got[0].Start), &start))
	requirements.NoError(json.Unmarshal([]byte(got[0].End), &end))
	checks.Equal(sourceKeyCursorCoordinate{Bound: "exclusive"}, start)
	checks.Equal(sourceKeyCursorCoordinate{Bound: "inclusive", SourceKey: "\u03bb/\u96ea\n\"quoted\""}, end)
}

func TestPersonFactSourceCursorsEncodeBackstopRange(t *testing.T) {
	key := CursorKey{PersonID: 7, SourceLane: SourceDocumentText,
		ProgramFingerprint: "program", CatalogFingerprint: "catalog"}
	got, _, err := PersonFactSourceCursors([]GenerationCursor{{
		Key: key, Mode: GenerationCursorBackstop, ReconcileToKey: "0009", BackstopUpperKey: "0009",
	}})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "person-sweep/v1/document_text/backstop", got[0].Lane)
}

func TestPersonFactSourceCursorsRejectInvalidRanges(t *testing.T) {
	key := CursorKey{PersonID: 7, SourceLane: SourceConversationText,
		ProgramFingerprint: "program", CatalogFingerprint: "catalog"}
	tests := map[string][]GenerationCursor{
		"optimistic empty":        {{Key: key, Mode: GenerationCursorOptimistic, CursorFrom: 4, CursorThrough: 4}},
		"optimistic keys":         {{Key: key, Mode: GenerationCursorOptimistic, CursorThrough: 4, ReconcileToKey: "x"}},
		"reconciliation integers": {{Key: key, Mode: GenerationCursorReconciliation, CursorThrough: 1, ReconcileToKey: "x"}},
		"reconciliation empty":    {{Key: key, Mode: GenerationCursorReconciliation}},
		"backstop descending":     {{Key: key, Mode: GenerationCursorBackstop, ReconcileFromKey: "z", ReconcileToKey: "a", BackstopUpperKey: "z"}},
		"duplicate": {
			{Key: key, Mode: GenerationCursorOptimistic, CursorThrough: 2},
			{Key: key, Mode: GenerationCursorOptimistic, CursorThrough: 2},
		},
		"overlap": {
			{Key: key, Mode: GenerationCursorOptimistic, CursorThrough: 3},
			{Key: key, Mode: GenerationCursorOptimistic, CursorFrom: 2, CursorThrough: 4},
		},
		"source key duplicate": {
			{Key: key, Mode: GenerationCursorReconciliation, ReconcileToKey: "b"},
			{Key: key, Mode: GenerationCursorReconciliation, ReconcileToKey: "b"},
		},
		"source key overlap": {
			{Key: key, Mode: GenerationCursorBackstop, ReconcileToKey: "m", BackstopUpperKey: "m"},
			{Key: key, Mode: GenerationCursorBackstop, ReconcileFromKey: "l", ReconcileToKey: "z", BackstopUpperKey: "z"},
		},
	}
	for name, cursors := range tests {
		t.Run(name, func(t *testing.T) {
			_, _, err := PersonFactSourceCursors(cursors)
			require.Error(t, err)
		})
	}
}

func TestValidatePersonFactCursorBindingRejectsGenerationIdentityMismatch(t *testing.T) {
	key := CursorKey{PersonID: 7, SourceLane: SourceConversationText,
		ProgramFingerprint: "program", CatalogFingerprint: "catalog"}
	envelope := []GenerationCursor{{Key: key, Mode: GenerationCursorOptimistic, CursorThrough: 1}}
	sources, hash, err := PersonFactSourceCursors(envelope)
	require.NoError(t, err)
	advance := []CursorAdvance{{Key: key, Mode: GenerationCursorOptimistic,
		NextSequence: 1, EnvelopeHash: hash}}
	base := personfacts.GenerationInput{PersonID: 7, ProgramFingerprint: "program",
		CatalogFingerprint: "catalog", SourceCursors: sources}
	for name, mutate := range map[string]func(*personfacts.GenerationInput){
		"person":  func(input *personfacts.GenerationInput) { input.PersonID++ },
		"program": func(input *personfacts.GenerationInput) { input.ProgramFingerprint += "-other" },
		"catalog": func(input *personfacts.GenerationInput) { input.CatalogFingerprint += "-other" },
	} {
		t.Run(name, func(t *testing.T) {
			generation := base
			mutate(&generation)
			_, err := ValidatePersonFactCursorBinding(generation, envelope, advance)
			require.ErrorContains(t, err, "does not match generation identity")
		})
	}
}

type workerFailureStore struct {
	cursor       Cursor
	lease        *Lease
	claimCalls   int
	started      []StartAttempt
	failed       []WorkFailure
	finalized    []FailureFinalization
	finished     []RunStatus
	cleanupAlive []bool
	gaps         []GapRequest
	gapResults   []GapResult
	reserved     []BudgetReservationRequest
	reserveErrAt int
	released     []BudgetReservation
	marked       []BudgetReservation
	renewCalls   atomic.Int64
}

func (s *workerFailureStore) StartPersonSweepRun(context.Context, StartRun) (Run, error) {
	return Run{}, nil
}

func (s *workerFailureStore) FinishPersonSweepRun(ctx context.Context, _ string, status RunStatus, _ time.Time) error {
	s.finished = append(s.finished, status)
	s.cleanupAlive = append(s.cleanupAlive, ctx.Err() == nil)
	return nil
}
func (s *workerFailureStore) StartPersonSweepAttempt(_ context.Context, input StartAttempt) error {
	s.started = append(s.started, input)
	return nil
}
func (s *workerFailureStore) ReconcilePersonSweepWorkContext(_ context.Context, request GapRequest) (GapResult, error) {
	index := len(s.gaps)
	s.gaps = append(s.gaps, request)
	if index < len(s.gapResults) {
		return s.gapResults[index], nil
	}
	return GapResult{}, nil
}
func (s *workerFailureStore) ClaimPersonSweep(context.Context, ClaimRequest) (*Lease, error) {
	s.claimCalls++
	if s.claimCalls == 1 && s.lease != nil {
		lease := *s.lease
		return &lease, nil
	}
	//nolint:nilnil // A nil lease is the production Store contract for no available work.
	return nil, nil
}
func (s *workerFailureStore) RenewPersonSweep(_ context.Context, lease Lease, _ time.Duration) (*Lease, error) {
	s.renewCalls.Add(1)
	return &lease, nil
}
func (s *workerFailureStore) EnsurePersonSweepCursors(_ context.Context, keys []CursorKey) ([]Cursor, error) {
	cursors := make([]Cursor, 0, len(keys))
	for _, key := range keys {
		cursor := s.cursor
		cursor.Key = key
		cursors = append(cursors, cursor)
	}
	return cursors, nil
}
func (s *workerFailureStore) ReservePersonSweepBudget(_ context.Context, request BudgetReservationRequest) (BudgetReservation, error) {
	s.reserved = append(s.reserved, request)
	if s.reserveErrAt > 0 && len(s.reserved) == s.reserveErrAt {
		return BudgetReservation{}, ErrBudgetExceeded
	}
	return BudgetReservation{ID: fmt.Sprintf("reservation-fixture-%d", len(s.reserved)), Request: request}, nil
}
func (s *workerFailureStore) ReleasePersonSweepBudget(_ context.Context, reservation BudgetReservation) error {
	s.released = append(s.released, reservation)
	return nil
}
func (s *workerFailureStore) MarkPersonSweepBudgetStarted(_ context.Context, reservation BudgetReservation) error {
	s.marked = append(s.marked, reservation)
	return nil
}
func (s *workerFailureStore) FailPersonSweepWork(ctx context.Context, failure WorkFailure) error {
	s.failed = append(s.failed, failure)
	s.cleanupAlive = append(s.cleanupAlive, ctx.Err() == nil)
	return nil
}
func (s *workerFailureStore) FinalizePersonSweepFailure(ctx context.Context, final FailureFinalization) error {
	s.finalized = append(s.finalized, final)
	s.cleanupAlive = append(s.cleanupAlive, ctx.Err() == nil)
	return nil
}

type workerFailureSource struct{}

func (workerFailureSource) LoadPersonSweepWindow(_ context.Context, request WindowRequest) (PersonWindow, error) {
	return PersonWindow{Changes: []ArchiveChange{{Sequence: 1, PersonID: request.PersonID,
		SourceLane: request.Lane}}, NextSequence: 1}, nil
}
func (workerFailureSource) LoadPersonFactState(context.Context, int64, personfacts.Catalog) (PersonFactState, error) {
	return PersonFactState{}, nil
}
func (workerFailureSource) BuildPersonSweepEvidenceStatusChanges(context.Context, int64, []ArchiveChange) ([]personfacts.EvidenceStatusChange, error) {
	return []personfacts.EvidenceStatusChange{{EvidenceKey: "fixture-evidence",
		SourceVersion: "source-v1", Supported: false,
		Reason: personfacts.EvidenceStatusSourceDeleted}}, nil
}
func (workerFailureSource) ListPersonSweepHistoricalCandidates(context.Context, HistoricalCandidateRequest) ([]int64, error) {
	return nil, nil
}
func (workerFailureSource) SearchPersonSweepMessages(context.Context, ContextRequest) ([]EvidenceItem, error) {
	return nil, nil
}
func (workerFailureSource) HydratePersonSweepMessages(context.Context, int64, []int64) ([]EvidenceItem, error) {
	return nil, nil
}
func (workerFailureSource) SearchPersonSweepDocuments(context.Context, DocumentContextRequest) ([]EvidenceItem, error) {
	return nil, nil
}

type workerFailureCatalog struct{ catalog personfacts.Catalog }

func (c workerFailureCatalog) BuildPersonFactCatalogContext(context.Context, bool) (personfacts.Catalog, error) {
	return c.catalog, nil
}

type workerFailureRunner struct{}

func (workerFailureRunner) PrepareStructured(context.Context, StructuredRequest) (PreparedStructuredRequest, error) {
	return PreparedStructuredRequest{}, errors.New("unexpected provider preparation")
}
func (workerFailureRunner) RunPreparedStructured(context.Context, PreparedStructuredRequest) (StructuredResponse, error) {
	return StructuredResponse{}, errors.New("unexpected provider call")
}
func (workerFailureRunner) RunStructured(context.Context, StructuredRequest) (StructuredResponse, error) {
	return StructuredResponse{}, errors.New("unexpected provider call")
}

type workerFailureSink struct{ err error }

func (s workerFailureSink) ApplyPersonSweep(context.Context, ApplyRequest) (ApplyResult, error) {
	return ApplyResult{}, s.err
}

func workerTestConfig(t *testing.T) (Config, personfacts.Catalog) {
	t.Helper()
	config := Config{Enabled: true, Provider: ProviderConfig{
		Kind: ProviderOpenAICompatible, Endpoint: "https://api.example.test/v1",
		Model: "gpt-test", APIKeyEnv: "TEST_KEY", RetentionPosture: "zero_retention",
		TrainingPosture: "no_training", AllowedSources: []SourceClass{SourceConversationText},
		SourceSince: "2025-01-01", RequestTimeout: time.Second,
	}}
	config.ApplyDefaults()
	catalog, err := personfacts.BuildCatalog(nil, personfacts.CatalogOptions{})
	require.NoError(t, err)
	return config, catalog
}

func TestPersonSweepWorkerFailureLeavesCursor(t *testing.T) {
	tests := []struct {
		name  string
		err   error
		class FailureClass
	}{
		{name: "timeout", err: context.DeadlineExceeded, class: FailureTimeout},
		{name: "invalid output", err: invalidOutputError{errors.New("invalid")}, class: FailureInvalidOutput},
		{name: "policy", err: ErrPersonSweepConsentRevoked, class: FailurePolicy},
		{name: "budget", err: ErrBudgetExceeded, class: FailureBudget},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			config, catalog := workerTestConfig(t)
			now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
			lastBackstop := now
			store := &workerFailureStore{cursor: Cursor{OptimisticSequence: 0,
				ReconciliationComplete: true, LastBackstopAt: &lastBackstop}}
			worker := Worker{Config: config, Store: store, Source: workerFailureSource{},
				Context: NewContextRetriever(workerFailureSource{}),
				Sink:    workerFailureSink{err: test.err}, Runner: workerFailureRunner{},
				Catalog: workerFailureCatalog{catalog: catalog}, Clock: func() time.Time { return now },
				NewID: func() string { return "attempt-fixture" }, WorkerID: "worker-fixture"}
			_, err := worker.RunPerson(t.Context(), "run-fixture", Lease{
				PersonID: 7, WorkerID: "worker-fixture", Fence: 1, ExpiresAt: now.Add(time.Hour),
			}, RunIncremental)
			requirements.ErrorIs(err, test.err)
			requirements.Len(store.finalized, 1)
			checks.Equal(test.class, store.finalized[0].Class)
			checks.Zero(store.cursor.OptimisticSequence)
		})
	}
}

type workerProductionSource struct {
	windows            map[GenerationCursorMode]PersonWindow
	windowsByLane      map[SourceClass]map[GenerationCursorMode]PersonWindow
	windowRequests     []WindowRequest
	historicalRequests []HistoricalCandidateRequest
	searches           []ContextRequest
	context            []EvidenceItem
}

func (s *workerProductionSource) LoadPersonSweepWindow(_ context.Context, request WindowRequest) (PersonWindow, error) {
	s.windowRequests = append(s.windowRequests, request)
	if windows := s.windowsByLane[request.Lane]; windows != nil {
		return windows[request.Mode], nil
	}
	return s.windows[request.Mode], nil
}
func (*workerProductionSource) LoadPersonFactState(context.Context, int64, personfacts.Catalog) (PersonFactState, error) {
	return PersonFactState{}, nil
}
func (*workerProductionSource) BuildPersonSweepEvidenceStatusChanges(context.Context, int64, []ArchiveChange) ([]personfacts.EvidenceStatusChange, error) {
	return nil, nil
}
func (s *workerProductionSource) ListPersonSweepHistoricalCandidates(_ context.Context, request HistoricalCandidateRequest) ([]int64, error) {
	s.historicalRequests = append(s.historicalRequests, request)
	ids := make([]int64, 0, len(s.context))
	for _, item := range s.context {
		ids = append(ids, item.Ref.MessageID)
	}
	return ids, nil
}
func (s *workerProductionSource) SearchPersonSweepMessages(_ context.Context, request ContextRequest) ([]EvidenceItem, error) {
	s.searches = append(s.searches, request)
	return append([]EvidenceItem(nil), s.context...), nil
}
func (*workerProductionSource) HydratePersonSweepMessages(context.Context, int64, []int64) ([]EvidenceItem, error) {
	return nil, nil
}
func (*workerProductionSource) SearchPersonSweepDocuments(context.Context, DocumentContextRequest) ([]EvidenceItem, error) {
	return nil, nil
}

type workerProductionRunner struct {
	cancel   context.CancelFunc
	started  chan<- struct{}
	block    <-chan struct{}
	prepared int
	ran      int
}

func (r *workerProductionRunner) PrepareStructured(_ context.Context, request StructuredRequest) (PreparedStructuredRequest, error) {
	r.prepared++
	return NewPreparedStructuredRequest(request, []byte(`{"wire":"prepared"}`))
}
func (r *workerProductionRunner) RunPreparedStructured(context.Context, PreparedStructuredRequest) (StructuredResponse, error) {
	r.ran++
	if r.started != nil {
		r.started <- struct{}{}
	}
	if r.block != nil {
		<-r.block
	}
	if r.cancel != nil {
		r.cancel()
	}
	return StructuredResponse{Output: json.RawMessage(`{"claims":[]}`),
		ProviderRequestID: "request-production", ProviderVersion: "provider-v1",
		ModelVersion: "model-v1", Usage: TokenUsage{InputTokens: 2, OutputTokens: 1}}, nil
}
func (r *workerProductionRunner) RunStructured(context.Context, StructuredRequest) (StructuredResponse, error) {
	return StructuredResponse{}, errors.New("worker must use prepared production path")
}

type workerProductionSink struct {
	requests []ApplyRequest
	err      error
}

func (s *workerProductionSink) ApplyPersonSweep(ctx context.Context, request ApplyRequest) (ApplyResult, error) {
	if err := ctx.Err(); err != nil {
		return ApplyResult{}, err
	}
	s.requests = append(s.requests, request)
	return ApplyResult{Mutations: ApplyMutationMetadata{ProjectionRowsWritten: 1}}, s.err
}

func TestPersonSweepWorkerHeartbeatsLeaseDuringProviderIO(t *testing.T) {
	store := &workerFailureStore{}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	runner := &workerProductionRunner{started: started, block: release}
	worker := Worker{Config: Config{LeaseDuration: 30 * time.Millisecond}, Store: store, Runner: runner}
	type result struct {
		lease Lease
		err   error
	}
	done := make(chan result, 1)
	go func() {
		lease, _, err := worker.runPreparedWithLeaseHeartbeat(t.Context(), Lease{
			PersonID: 7, WorkerID: "worker-fixture", Fence: 1,
		}, PreparedStructuredRequest{})
		done <- result{lease: lease, err: err}
	}()
	<-started
	require.Eventually(t, func() bool { return store.renewCalls.Load() >= 2 },
		500*time.Millisecond, 5*time.Millisecond)
	close(release)
	got := <-done
	require.NoError(t, got.err)
	assert.Equal(t, int64(1), got.lease.Fence)
}

func TestPersonSweepWorkerCanceledProviderUsesDetachedCleanup(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	config, catalog := workerTestConfig(t)
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	lastBackstop := now
	lease := Lease{PersonID: 7, WorkerID: "worker-fixture", Fence: 1,
		ExpiresAt: now.Add(time.Hour)}
	store := &workerFailureStore{lease: &lease, cursor: Cursor{OptimisticSequence: 0,
		ReconciliationComplete: true, LastBackstopAt: &lastBackstop}}
	seed := packetTestEvidence(10, SourceConversationText, "changed seed")
	source := &workerProductionSource{windows: map[GenerationCursorMode]PersonWindow{
		GenerationCursorOptimistic: {Seeds: []EvidenceItem{seed}, Changes: []ArchiveChange{{
			Sequence: 1, PersonID: 7, SourceLane: SourceConversationText}}, NextSequence: 1},
	}}
	ctx, cancel := context.WithCancel(t.Context())
	runner := &workerProductionRunner{cancel: cancel}
	worker := Worker{Config: config, Store: store, Source: source,
		Context: NewContextRetriever(source),
		Sink:    &workerProductionSink{}, Runner: runner, Catalog: workerFailureCatalog{catalog: catalog},
		Clock: func() time.Time { return now }, NewID: func() string {
			if len(store.started) == 0 {
				return "run-cancelled"
			}
			return "attempt-cancelled"
		}, WorkerID: "worker-fixture"}
	_, err := worker.Run(ctx, RunRequest{Kind: RunManual, Mode: RunIncremental, Limit: 1})
	requirements.ErrorIs(err, context.Canceled)
	requirements.Len(store.finalized, 1)
	checks.Equal(FailureTimeout, store.finalized[0].Class)
	checks.Equal([]bool{true, true}, store.cleanupAlive,
		"failure accounting and run completion must use live detached contexts")
}

func TestPersonSweepWorkerRunBackstopForcesFreshAssembly(t *testing.T) {
	config, catalog := workerTestConfig(t)
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	store := &workerFailureStore{cursor: Cursor{OptimisticSequence: 0,
		ReconciliationComplete: true, LastBackstopAt: &now}}
	seed := packetTestEvidence(11, SourceConversationText, "optimistic seed")
	source := &workerProductionSource{windows: map[GenerationCursorMode]PersonWindow{
		GenerationCursorOptimistic: {Seeds: []EvidenceItem{seed}, Changes: []ArchiveChange{{
			Sequence: 1, PersonID: 7, SourceLane: SourceConversationText}}, NextSequence: 1},
		GenerationCursorBackstop: {CapturedUpperKey: "0001", NextReconcileKey: "0001", ReconciliationDone: true},
	}}
	worker := Worker{Config: config, Store: store, Source: source,
		Context: NewContextRetriever(source),
		Sink:    &workerProductionSink{err: errors.New("stop after assembly")}, Runner: &workerProductionRunner{},
		Catalog: workerFailureCatalog{catalog: catalog}, Clock: func() time.Time { return now },
		NewID: func() string { return "attempt-backstop" }, WorkerID: "worker-fixture"}
	_, err := worker.RunPerson(t.Context(), "run-backstop", Lease{PersonID: 7,
		WorkerID: "worker-fixture", Fence: 1, ExpiresAt: now.Add(time.Hour)}, RunBackstop)
	require.Error(t, err)
	require.Len(t, store.started, 1)
	modes := make([]GenerationCursorMode, 0, len(store.started[0].CursorEnvelope))
	for _, cursor := range store.started[0].CursorEnvelope {
		modes = append(modes, cursor.Mode)
	}
	assert.Contains(t, modes, GenerationCursorBackstop,
		"explicit RunBackstop must ignore a recent backstop timestamp")
}

func TestPersonSweepWorkerRunBackstopForcesWorkSelection(t *testing.T) {
	config, catalog := workerTestConfig(t)
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	store := &workerFailureStore{}
	source := &workerProductionSource{}
	worker := Worker{Config: config, Store: store, Source: source,
		Context: NewContextRetriever(source), Sink: &workerProductionSink{},
		Runner: &workerProductionRunner{}, Catalog: workerFailureCatalog{catalog: catalog},
		Clock: func() time.Time { return now }, NewID: func() string { return "run-force-backstop" },
		WorkerID: "worker-fixture"}
	_, err := worker.Run(t.Context(), RunRequest{Kind: RunManual, Mode: RunBackstop, Limit: 1})
	require.NoError(t, err)
	require.Len(t, store.gaps, 1)
	assert.True(t, store.gaps[0].ForceBackstop)
}

func TestPersonSweepWorkerReconciliationContinuesAfterCappedPage(t *testing.T) {
	config, catalog := workerTestConfig(t)
	config.WorkBatchSize = 1_001
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	store := &workerFailureStore{gapResults: []GapResult{
		{PeopleScanned: 1_000, NextPersonID: 1_000},
		{PeopleScanned: 1, NextPersonID: 1_001},
		{},
	}}
	source := workerFailureSource{}
	worker := Worker{Config: config, Store: store, Source: source,
		Context: NewContextRetriever(source), Sink: workerFailureSink{}, Runner: workerFailureRunner{},
		Catalog: workerFailureCatalog{catalog: catalog}, Clock: func() time.Time { return now },
		NewID: func() string { return "run-capped-page" }, WorkerID: "worker-fixture"}

	_, err := worker.Run(t.Context(), RunRequest{Kind: RunManual, Mode: RunIncremental, Limit: 1})
	require.NoError(t, err)
	require.Len(t, store.gaps, 3)
	assert.Equal(t, []int64{0, 1_000, 1_001}, []int64{
		store.gaps[0].AfterPersonID,
		store.gaps[1].AfterPersonID,
		store.gaps[2].AfterPersonID,
	})
}

func TestPersonSweepWorkerProductionAssemblerRetrievesContext(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	config, catalog := workerTestConfig(t)
	config.ChangeBatchSize = 37
	config.HistoricalMessageCap = 41
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	store := &workerFailureStore{cursor: Cursor{ReconciliationComplete: true, LastBackstopAt: &now}}
	seed := packetTestEvidence(12, SourceConversationText, "changed seed")
	contextItem := packetTestEvidence(13, SourceConversationText, "historical context")
	source := &workerProductionSource{windows: map[GenerationCursorMode]PersonWindow{
		GenerationCursorOptimistic: {Seeds: []EvidenceItem{seed}, Changes: []ArchiveChange{{
			Sequence: 1, PersonID: 7, SourceLane: SourceConversationText}}, NextSequence: 1},
	}, context: []EvidenceItem{contextItem}}
	runner := &workerProductionRunner{}
	worker := Worker{Config: config, Store: store, Source: source,
		Context: NewContextRetriever(source),
		Sink:    &workerProductionSink{}, Runner: runner,
		Catalog: workerFailureCatalog{catalog: catalog}, Clock: func() time.Time { return now },
		NewID: func() string { return "attempt-context" }, WorkerID: "worker-fixture"}
	_, err := worker.RunPerson(t.Context(), "run-context", Lease{PersonID: 7,
		WorkerID: "worker-fixture", Fence: 1, ExpiresAt: now.Add(time.Hour)}, RunIncremental)
	requirements.NoError(err)
	checks.Len(source.searches, len(catalog.Targets))
	requirements.NotEmpty(source.windowRequests)
	checks.Equal(37, source.windowRequests[0].Limit)
	requirements.NotEmpty(source.historicalRequests)
	checks.Equal(41, source.historicalRequests[0].Limit)
	requirements.Len(store.reserved, 1)
	requirements.Len(store.marked, 1)
	checks.Equal(store.reserved[0].InputHash, store.marked[0].Request.InputHash)
	checks.Equal(1, runner.prepared)
	checks.Equal(1, runner.ran)
}

func TestPersonSweepWorkerSharesRequestBudgetAcrossSourceLanes(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	config, catalog := workerTestConfig(t)
	config.Provider.AllowedSources = []SourceClass{SourceConversationText, SourceMeetingText}
	config.Budgets.MaxRequestsPerPerson = 1
	config.Budgets.MaxOutputTokensPerPerson = extractionMaxOutputTokens
	config.EvidenceMaxItems = 1
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	store := &workerFailureStore{cursor: Cursor{
		ReconciliationComplete: true, LastBackstopAt: &now,
	}}
	source := &workerProductionSource{windowsByLane: map[SourceClass]map[GenerationCursorMode]PersonWindow{
		SourceConversationText: {GenerationCursorOptimistic: {
			Seeds:        []EvidenceItem{packetTestEvidence(21, SourceConversationText, "conversation seed")},
			Changes:      []ArchiveChange{{Sequence: 1, PersonID: 7, SourceLane: SourceConversationText}},
			NextSequence: 1,
		}},
		SourceMeetingText: {GenerationCursorOptimistic: {
			Seeds:        []EvidenceItem{packetTestEvidence(22, SourceMeetingText, "meeting seed")},
			Changes:      []ArchiveChange{{Sequence: 2, PersonID: 7, SourceLane: SourceMeetingText}},
			NextSequence: 2,
		}},
	}}
	runner := &workerProductionRunner{}
	sink := &workerProductionSink{}
	worker := Worker{Config: config, Store: store, Source: source,
		Context: NewContextRetriever(source), Sink: sink, Runner: runner,
		Catalog: workerFailureCatalog{catalog: catalog}, Clock: func() time.Time { return now },
		NewID: func() string { return "attempt-shared-budget" }, WorkerID: "worker-fixture"}

	_, err := worker.RunPerson(t.Context(), "run-shared-budget", Lease{PersonID: 7,
		WorkerID: "worker-fixture", Fence: 1, ExpiresAt: now.Add(time.Hour)}, RunIncremental)
	requirements.NoError(err)
	checks.Equal(1, runner.ran)
	requirements.Len(store.reserved, 1)
	requirements.Len(store.started, 1)
	requirements.Len(store.started[0].CursorEnvelope, 1)
	checks.Equal(SourceConversationText, store.started[0].CursorEnvelope[0].Key.SourceLane)
	requirements.Len(sink.requests, 1)
	checks.True(sink.requests[0].DeferredCursorWork,
		"the unprocessed source lane must keep durable work available")
}

func TestPersonSweepWorkerRequeuesUnprocessedForcedBackstopLane(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	config, catalog := workerTestConfig(t)
	config.Provider.AllowedSources = []SourceClass{SourceConversationText, SourceMeetingText}
	config.Budgets.MaxRequestsPerPerson = 1
	config.Budgets.MaxOutputTokensPerPerson = extractionMaxOutputTokens
	config.EvidenceMaxItems = 1
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	store := &workerFailureStore{cursor: Cursor{ReconciliationComplete: true}}
	backstopWindow := func(item EvidenceItem, key string) PersonWindow {
		return PersonWindow{Seeds: []EvidenceItem{item}, Changes: []ArchiveChange{{
			PersonID: 7, SourceLane: item.SourceClass, MessageID: item.Ref.MessageID,
		}}, CapturedUpperKey: key, NextReconcileKey: key, ReconciliationDone: true}
	}
	source := &workerProductionSource{windowsByLane: map[SourceClass]map[GenerationCursorMode]PersonWindow{
		SourceConversationText: {GenerationCursorBackstop: backstopWindow(
			packetTestEvidence(41, SourceConversationText, "conversation backstop"), "00000000000000000041")},
		SourceMeetingText: {GenerationCursorBackstop: backstopWindow(
			packetTestEvidence(42, SourceMeetingText, "meeting backstop"), "00000000000000000042")},
	}}
	sink := &workerProductionSink{}
	worker := Worker{Config: config, Store: store, Source: source,
		Context: NewContextRetriever(source), Sink: sink, Runner: &workerProductionRunner{},
		Catalog: workerFailureCatalog{catalog: catalog}, Clock: func() time.Time { return now },
		NewID: func() string { return "attempt-bounded-backstop" }, WorkerID: "worker-fixture"}

	_, err := worker.RunPerson(t.Context(), "run-bounded-backstop", Lease{PersonID: 7,
		WorkerID: "worker-fixture", Fence: 1, ExpiresAt: now.Add(time.Hour)}, RunBackstop)
	must.NoError(err)
	must.Len(sink.requests, 1)
	checks.True(sink.requests[0].DeferredCursorWork)
	must.Len(sink.requests[0].CursorEnvelope, 1)
	checks.Equal(GenerationCursorBackstop, sink.requests[0].CursorEnvelope[0].Mode)
}

func TestPersonSweepWorkerReservesEveryBatchBeforeProviderCall(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	config, catalog := workerTestConfig(t)
	config.Budgets.MaxRequestsPerPerson = 2
	config.Budgets.MaxOutputTokensPerPerson = 2 * extractionMaxOutputTokens
	config.EvidenceMaxItems = 1
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	store := &workerFailureStore{cursor: Cursor{
		ReconciliationComplete: true, LastBackstopAt: &now,
	}, reserveErrAt: 2}
	source := &workerProductionSource{windows: map[GenerationCursorMode]PersonWindow{
		GenerationCursorOptimistic: {
			Seeds: []EvidenceItem{
				packetTestEvidence(31, SourceConversationText, "first seed"),
				packetTestEvidence(32, SourceConversationText, "second seed"),
			},
			Changes:      []ArchiveChange{{Sequence: 2, PersonID: 7, SourceLane: SourceConversationText}},
			NextSequence: 2,
		},
	}}
	runner := &workerProductionRunner{}
	worker := Worker{Config: config, Store: store, Source: source,
		Context: NewContextRetriever(source), Sink: &workerProductionSink{}, Runner: runner,
		Catalog: workerFailureCatalog{catalog: catalog}, Clock: func() time.Time { return now },
		NewID: func() string { return "attempt-preflight-budget" }, WorkerID: "worker-fixture"}

	_, err := worker.RunPerson(t.Context(), "run-preflight-budget", Lease{PersonID: 7,
		WorkerID: "worker-fixture", Fence: 1, ExpiresAt: now.Add(time.Hour)}, RunIncremental)
	requirements.ErrorIs(err, ErrBudgetExceeded)
	checks.Equal(2, runner.prepared)
	checks.Zero(runner.ran, "no paid provider call may precede full-batch budget admission")
	requirements.Len(store.released, 1)
	checks.Equal("reservation-fixture-1", store.released[0].ID)
}

func TestPersonSweepWorkerNoChangedSeedAdvancesCursorWithoutProvider(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	config, catalog := workerTestConfig(t)
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	store := &workerFailureStore{cursor: Cursor{ReconciliationComplete: true, LastBackstopAt: &now}}
	source := &workerProductionSource{windows: map[GenerationCursorMode]PersonWindow{
		GenerationCursorOptimistic: {Changes: []ArchiveChange{{Sequence: 1,
			PersonID: 7, SourceLane: SourceConversationText}}, NextSequence: 1},
	}}
	runner := &workerProductionRunner{}
	sink := &workerProductionSink{}
	worker := Worker{Config: config, Store: store, Source: source,
		Context: NewContextRetriever(source),
		Sink:    sink, Runner: runner,
		Catalog: workerFailureCatalog{catalog: catalog}, Clock: func() time.Time { return now },
		NewID: func() string { return "unused-attempt" }, WorkerID: "worker-fixture"}
	result, err := worker.RunPerson(t.Context(), "run-pre-attempt", Lease{PersonID: 7,
		WorkerID: "worker-fixture", Fence: 1, ExpiresAt: now.Add(time.Hour)}, RunIncremental)
	requirements.NoError(err)
	requirements.Len(store.started, 1)
	checks.Empty(store.failed)
	checks.Empty(store.reserved)
	checks.Zero(runner.prepared)
	checks.Zero(runner.ran)
	requirements.Len(sink.requests, 1)
	request := sink.requests[0]
	checks.Equal(StatusOnlyProvider, request.Generation.Provider)
	checks.Empty(request.Generation.Claims)
	checks.Empty(request.Generation.EvidenceStatusChanges)
	checks.Empty(request.Batches)
	checks.Equal(Usage{}, request.Usage)
	requirements.Len(request.CursorAdvances, 1)
	checks.Equal(int64(1), request.CursorAdvances[0].NextSequence)
	checks.Equal(request.CursorAdvances, result.CursorAdvances)
}
