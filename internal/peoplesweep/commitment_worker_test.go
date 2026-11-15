package peoplesweep

import (
	"context"
	"encoding/json"
	"errors"
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
	if input.PersonID > 0 && len(input.ExcludePersonIDs) > 0 {
		return nil, errors.New("targeted claims cannot contain exclusions")
	}
	if len(s.claims) > 1 {
		return nil, nil //nolint:nilnil // A nil lease means no work under the ClaimPersonSweep contract.
	}
	return s.lease, nil
}
func (*commitmentWorkerFixture) HasActivePersonCommitmentConsent(context.Context, string) (bool, error) {
	return true, nil
}
func (*commitmentWorkerFixture) PersonCommitmentsDue(context.Context, int64, time.Time, time.Duration, time.Duration, string, int) (bool, error) {
	return true, nil
}
func (s *commitmentWorkerFixture) LoadPersonCommitmentWindow(_ context.Context, id int64, _ ProviderProfile, _ CommitmentConfig, now time.Time) (CommitmentWindow, error) {
	if s.window != nil {
		return *s.window, nil
	}
	return CommitmentWindow{PersonID: id, Now: now}, nil
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

func TestTargetedCommitmentOnlyRunSkipsOrdinaryWork(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	worker, st, sink := newConsentOnlyCommitmentWorker(t)
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
	a.Empty(sink.requests[0].Batches)
}

type commitmentCallAuthority struct {
	workerRepairAuthority
}

func (*commitmentCallAuthority) HasActivePersonCommitmentConsent(context.Context, string) (bool, error) {
	return true, nil
}

func TestCommitmentOptionalCallRepairAndBudget(t *testing.T) {
	for _, mode := range []string{"repair", "budget"} {
		t.Run(mode, func(t *testing.T) {
			r, a := require.New(t), assert.New(t)
			cfg, catalog := workerTestConfig(t)
			mutateTestProvider(&cfg, func(p *ProviderConfig) { p.AllowSensitive = true })
			cfg.Commitments.Enabled = true
			now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
			window := CommitmentWindow{PersonID: 7, Now: now, Sources: []CommitmentSource{{ID: "m71", MessageID: 71, Direction: CommitmentOwedTo, Text: "I will send it.", EventTime: now.Add(-time.Hour)}}}
			st := &commitmentWorkerFixture{window: &window}
			if mode == "budget" {
				st.reserveErrAt = 2
			}
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
			if mode == "budget" {
				a.Equal(FailureBudget, class)
				a.Equal(1, driver.calls)
				a.Len(accounting.completedBatches, 1)
			} else {
				a.Equal(2, driver.calls)
				a.Len(accounting.completedBatches, 2)
			}
			if mode == "repair" {
				r.NotNil(result)
				a.Empty(class)
			} else {
				a.Nil(result)
			}
		})
	}
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
