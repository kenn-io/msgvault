package peoplesweep

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type commitmentWorkerFixture struct {
	workerFailureStore

	due      bool
	claims   []ClaimRequest
	deferred int
}

func (s *commitmentWorkerFixture) ClaimPersonSweep(_ context.Context, input ClaimRequest) (*Lease, error) {
	s.claims = append(s.claims, input)
	if input.PersonID > 0 && len(input.ExcludePersonIDs) > 0 {
		return nil, errors.New("targeted claims cannot contain exclusions")
	}
	if len(s.claims) > 1 {
		return nil, nil
	}
	return s.lease, nil
}
func (*commitmentWorkerFixture) HasActivePersonCommitmentConsent(context.Context, string) (bool, error) {
	return true, nil
}
func (s *commitmentWorkerFixture) PersonCommitmentsDue(context.Context, int64, time.Time, time.Duration, time.Duration, ...string) (bool, error) {
	return s.due, nil
}
func (*commitmentWorkerFixture) LoadPersonCommitmentWindow(_ context.Context, id int64, _ ProviderProfile, _ CommitmentConfig, now time.Time, zone string) (CommitmentWindow, error) {
	return CommitmentWindow{PersonID: id, Now: now, Timezone: zone}, nil
}
func (*commitmentWorkerFixture) EnsurePersonSweepWork(context.Context, int64, bool) (bool, error) {
	return true, nil
}
func (s *commitmentWorkerFixture) DeferPersonSweepAttempt(context.Context, Lease, string, time.Time) error {
	s.deferred++
	return nil
}

type commitmentOnlyRunner struct{ workerFailureRunner }

func (commitmentOnlyRunner) OrdinaryInferenceAllowed(context.Context) (bool, error) {
	return false, nil
}

func TestTargetedCommitmentOnlySweepSucceedsAndDefersImmediateRerun(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	cfg, catalog := workerTestConfig(t)
	mutateTestProvider(&cfg, func(provider *ProviderConfig) { provider.AllowSensitive = true })
	cfg.Commitments.Enabled = true
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	st := &commitmentWorkerFixture{due: true, workerFailureStore: workerFailureStore{
		cursor: Cursor{ReconciliationComplete: true, LastBackstopAt: &now},
		lease:  &Lease{PersonID: 7, WorkerID: "commitment-worker", Fence: 1, ExpiresAt: now.Add(time.Hour)},
	}}
	seed := packetTestEvidence(71, SourceConversationText, "pending ordinary evidence")
	source := &workerProductionSource{windows: map[GenerationCursorMode]PersonWindow{
		GenerationCursorOptimistic: {Seeds: []EvidenceItem{seed}, Changes: []ArchiveChange{{Sequence: 1, PersonID: 7, SourceLane: SourceConversationText}}, NextSequence: 1},
	}}
	sink := &workerProductionSink{}
	worker := Worker{Config: cfg, Store: st, Source: source, Context: NewContextRetriever(source), Sink: sink,
		Runner: commitmentOnlyRunner{}, Catalog: workerFailureCatalog{catalog: catalog}, Clock: func() time.Time { return now },
		NewID: func() string { return "commitment-attempt" }, WorkerID: "commitment-worker"}
	result, err := worker.Run(t.Context(), RunRequest{Kind: RunManual, Mode: RunIncremental, PersonID: 7})
	r.NoError(err)
	a.Equal(1, result.PeopleSucceeded)
	r.Len(st.claims, 1)
	a.Empty(st.claims[0].ExcludePersonIDs)
	r.Len(sink.requests, 1)
	a.True(sink.requests[0].CommitmentOnly)
	a.Empty(sink.requests[0].Batches)
	a.Zero(st.cursor.OptimisticSequence)
	st.due, st.claims = false, nil
	result, err = worker.Run(t.Context(), RunRequest{Kind: RunManual, Mode: RunIncremental, PersonID: 7})
	r.NoError(err)
	a.Zero(result.PeopleSucceeded)
	a.Equal(1, st.deferred)
	a.Empty(st.finalized)
	a.Empty(st.failed)
	a.Len(sink.requests, 1)
	a.Zero(st.cursor.OptimisticSequence)
}
