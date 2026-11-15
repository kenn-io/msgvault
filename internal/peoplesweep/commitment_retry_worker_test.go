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

func (s *retryCommitmentStore) LoadPersonCommitmentWindow(_ context.Context, id int64, _ ProviderProfile, _ CommitmentConfig, now time.Time, zone string) (CommitmentWindow, error) {
	return CommitmentWindow{PersonID: id, Now: now, Timezone: zone, Sources: []CommitmentSource{{ID: "m1", MessageID: 1, Direction: CommitmentOwedTo, Text: "I will call.", EventTime: now.Add(-time.Hour)}}}, nil
}

type retryCommitmentSink struct {
	workerProductionSink
	store *retryCommitmentStore
}

func (s *retryCommitmentSink) ApplyPersonSweep(_ context.Context, request ApplyRequest) (ApplyResult, error) {
	s.requests = append(s.requests, request)
	if request.Lease.PersonID == 7 {
		return ApplyResult{CommitmentFailureClass: FailureInternal}, nil
	}
	s.store.completed = append(s.store.completed, request.Lease.PersonID)
	return ApplyResult{}, nil
}

type retryCommitmentRunner struct{ *Runner }

func (retryCommitmentRunner) OrdinaryInferenceAllowed(context.Context) (bool, error) {
	return false, nil
}

func TestPersonSweepWorkerDefersCommitmentPrepareFailureForTheRestOfRun(t *testing.T) {
	worker, fixture, _ := newConsentOnlyCommitmentWorker(t)
	st := &retryCommitmentStore{commitmentWorkerFixture: fixture}
	sink := &retryCommitmentSink{store: st}
	response := briefDriverResponse(json.RawMessage(`{"promises":[]}`), "request")
	driver := &workerRepairDriver{responses: []DriverResponse{response, response}}
	runner := newWorkerRepairRunnerWithDependencies(t, worker.Config, driver, &commitmentCallAuthority{}, NewCredentialResolver(nil, func(string) (string, bool) { return "test-key", true }))
	worker.Store, worker.Sink, worker.Runner = st, sink, retryCommitmentRunner{runner}
	result, err := worker.Run(t.Context(), RunRequest{Kind: RunManual, Mode: RunIncremental, Brief: BriefModeSkip})
	require.NoError(t, err)
	assert.Equal(t, 2, driver.calls)
	require.GreaterOrEqual(t, len(st.claims), 2)
	assert.Contains(t, st.claims[1].ExcludePersonIDs, int64(7))
	assert.Equal(t, []int64{8}, st.completed)
	require.Len(t, result.People, 2)
	assert.Equal(t, FailureInternal, result.People[0].CommitmentFailureClass)
}
