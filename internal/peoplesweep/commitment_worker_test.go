package peoplesweep

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type commitmentWorkerFixture struct {
	workerFailureStore

	window *CommitmentWindow
	claims []ClaimRequest
}

func (s *commitmentWorkerFixture) ClaimPersonSweep(_ context.Context, input ClaimRequest) (*Lease, error) {
	s.claims = append(s.claims, input)
	if len(s.claims) > 1 {
		return nil, nil //nolint:nilnil // A nil lease means no work under the ClaimPersonSweep contract.
	}
	return s.lease, nil
}
func (*commitmentWorkerFixture) HasActivePersonCommitmentConsent(context.Context, string) (bool, error) {
	return true, nil
}
func (*commitmentWorkerFixture) PersonCommitmentsDue(context.Context, int64, time.Time, time.Duration, time.Duration, string) (bool, error) {
	return true, nil
}
func (s *commitmentWorkerFixture) LoadPersonCommitmentWindow(_ context.Context, id int64, _ ProviderProfile, cfg CommitmentConfig, now time.Time) (CommitmentWindow, error) {
	if s.window != nil {
		window := *s.window
		window.Timezone = cfg.Timezone
		return window, nil
	}
	return CommitmentWindow{PersonID: id, Now: now, Timezone: cfg.Timezone}, nil
}
func (*commitmentWorkerFixture) EnsurePersonSweepWork(context.Context, int64, bool) (bool, error) {
	return true, nil
}
func (*commitmentWorkerFixture) DeferPersonSweepAttempt(context.Context, Lease, string, time.Time) error {
	return nil
}

type commitmentOnlyRunner struct{ workerFailureRunner }

func (commitmentOnlyRunner) OrdinaryInferenceAllowed(context.Context) (bool, error) {
	return false, nil
}

func TestTargetedCommitmentOnlyRunSkipsOrdinaryWorkAndPassesTimezone(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	worker, st, sink := newConsentOnlyCommitmentWorker(t)
	worker.Timezone = "America/New_York"
	seed := packetTestEvidence(71, SourceConversationText, "pending ordinary evidence")
	source := &workerProductionSource{windows: map[GenerationCursorMode]PersonWindow{
		GenerationCursorOptimistic: {Seeds: []EvidenceItem{seed}, Changes: []ArchiveChange{{Sequence: 1, PersonID: 7, SourceLane: SourceConversationText}}, NextSequence: 1},
	}}
	worker.Source, worker.Context = source, NewContextRetriever(source)
	result, err := worker.Run(t.Context(), RunRequest{Kind: RunManual, Mode: RunIncremental, PersonID: 7})
	r.NoError(err)
	a.Equal(1, result.PeopleSucceeded)
	r.Len(st.claims, 1)
	r.Len(sink.requests, 1)
	a.True(sink.requests[0].CommitmentOnly)
	r.NotNil(sink.requests[0].Commitment)
	a.Equal("America/New_York", sink.requests[0].Commitment.Window.Timezone)
	a.Empty(sink.requests[0].Batches)
}

type commitmentCallAuthority struct {
	workerRepairAuthority
}

func (*commitmentCallAuthority) HasActivePersonCommitmentConsent(context.Context, string) (bool, error) {
	return true, nil
}

func TestCommitmentOptionalCallRepair(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	cfg, catalog := workerTestConfig(t)
	mutateTestProvider(&cfg, func(p *ProviderConfig) { p.AllowSensitive = true })
	cfg.Commitments.Enabled = true
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	window := CommitmentWindow{PersonID: 7, Now: now, Sources: []CommitmentSource{{ID: "m71", MessageID: 71, Direction: CommitmentOwedTo, Text: "I will send it.", EventTime: now.Add(-time.Hour)}}}
	st := &commitmentWorkerFixture{window: &window}
	authority := &commitmentCallAuthority{}
	second := json.RawMessage(`{"promises":[]}`)
	driver := &workerRepairDriver{responses: []DriverResponse{briefDriverResponse(json.RawMessage(`{"promises":"bad"}`), "primary"), briefDriverResponse(second, "repair")}}
	runner := newWorkerRepairRunnerWithDependencies(t, cfg, driver, authority, NewCredentialResolver(nil, func(string) (string, bool) { return "test-key", true }))
	profile, err := cfg.Profile()
	r.NoError(err)
	lease := Lease{PersonID: 7, WorkerID: "worker-fixture", Fence: 1, ExpiresAt: now.Add(time.Hour)}
	worker := Worker{Config: cfg, Store: st, Runner: runner, Clock: func() time.Time { return now }}
	accounting := &sweepCallAccounting{budget: cfg.Budgets}
	var reservations []BudgetReservation
	result, class, _, err := worker.runCommitmentCall(t.Context(), briefCall{runID: "run", attemptID: "attempt", lease: lease, catalog: catalog, profile: profile, resolvedAt: now}, accounting, &reservations)
	r.NoError(err)
	r.Len(st.reserved, 2)
	a.Equal(ProviderCallPurposeCommitment, st.reserved[0].Purpose)
	a.Equal(ProviderCallPurposeCommitmentRepair, st.reserved[1].Purpose)
	a.Equal(1, st.reserved[1].CallOrdinal)
	r.NotNil(result)
	a.Empty(class)
}

func newConsentOnlyCommitmentWorker(t *testing.T) (Worker, *commitmentWorkerFixture, *workerProductionSink) {
	t.Helper()
	cfg, catalog := workerTestConfig(t)
	mutateTestProvider(&cfg, func(provider *ProviderConfig) { provider.AllowSensitive = true })
	cfg.Commitments.Enabled = true
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	st := &commitmentWorkerFixture{}
	st.cursor = Cursor{ReconciliationComplete: true, LastBackstopAt: &now}
	st.lease = &Lease{PersonID: 7, WorkerID: "consent-worker", Fence: 1, ExpiresAt: now.Add(time.Hour)}
	sink := &workerProductionSink{}
	worker := Worker{Config: cfg, Store: st, Source: workerFailureSource{},
		Context: NewContextRetriever(workerFailureSource{}), Sink: sink, Runner: commitmentOnlyRunner{},
		Catalog: workerFailureCatalog{catalog: catalog}, Clock: func() time.Time { return now },
		NewID: func() string { return "consent-attempt" }, WorkerID: "consent-worker"}
	return worker, st, sink
}

type retryCommitmentStore struct {
	*commitmentWorkerFixture

	completed []int64
}

func (s *retryCommitmentStore) ClaimPersonSweep(_ context.Context, request ClaimRequest) (*Lease, error) {
	s.claims = append(s.claims, request)
	for _, id := range []int64{7, 8} {
		if !slices.Contains(request.ExcludePersonIDs, id) && !slices.Contains(s.completed, id) {
			lease := *s.lease
			lease.PersonID = id
			return &lease, nil
		}
	}
	return nil, nil //nolint:nilnil // No work remains to claim.
}

func (s *retryCommitmentStore) LoadPersonCommitmentWindow(_ context.Context, id int64, _ ProviderProfile, _ CommitmentConfig, now time.Time) (CommitmentWindow, error) {
	return CommitmentWindow{PersonID: id, Now: now, Sources: []CommitmentSource{{ID: "m1", MessageID: 1, Direction: CommitmentOwedTo, Text: "I will call.", EventTime: now.Add(-time.Hour)}}}, nil
}

type retryCommitmentSink struct {
	workerProductionSink

	store *retryCommitmentStore
}

func (s *retryCommitmentSink) ApplyPersonSweep(_ context.Context, request ApplyRequest) (ApplyResult, error) {
	s.requests = append(s.requests, request)
	if request.Lease.PersonID == 7 {
		return ApplyResult{CommitmentFailureClass: FailureArchiveGap}, nil
	}
	s.store.completed = append(s.store.completed, request.Lease.PersonID)
	return ApplyResult{}, nil
}

func TestPersonSweepWorkerDefersCommitmentFailureForTheRestOfRun(t *testing.T) {
	worker, fixture, _ := newConsentOnlyCommitmentWorker(t)
	st := &retryCommitmentStore{commitmentWorkerFixture: fixture}
	sink := &retryCommitmentSink{store: st}
	response := briefDriverResponse(json.RawMessage(`{"promises":[]}`), "request")
	driver := &workerRepairDriver{responses: []DriverResponse{response, response}}
	runner := newWorkerRepairRunnerWithDependencies(t, worker.Config, driver, &commitmentCallAuthority{}, NewCredentialResolver(nil, func(string) (string, bool) { return "test-key", true }))
	source := &workerProductionSource{windows: map[GenerationCursorMode]PersonWindow{
		GenerationCursorOptimistic: {Changes: []ArchiveChange{{Sequence: 1, PersonID: 7, SourceLane: SourceConversationText}}, NextSequence: 1},
	}}
	worker.Store, worker.Sink, worker.Runner = st, sink, runner
	worker.Source, worker.Context = source, NewContextRetriever(source)
	result, err := worker.Run(t.Context(), RunRequest{Kind: RunManual, Mode: RunIncremental, Brief: BriefModeSkip})
	require.NoError(t, err)
	assert.Equal(t, 2, driver.calls)
	require.GreaterOrEqual(t, len(st.claims), 2)
	assert.Contains(t, st.claims[1].ExcludePersonIDs, int64(7))
	assert.Equal(t, []int64{8}, st.completed)
	require.Len(t, result.People, 2)
}
