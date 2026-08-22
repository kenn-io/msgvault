package visual

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestVisualPublicationCleanupFailureDoesNotHideSuccessfulCommit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, generation, work := workerFixture(t, 1)
	backend := newFakeVisualBackend()
	worker := newTestVisualWorker(t, f, &fakeVisualProvider{}, backend)
	first, err := worker.Run(t.Context(), work)
	require.NoError(err)
	require.Equal(int64(1), first.Published)
	publication, err := f.Store.GetVisualPublication(t.Context(), generation.ID, work[0].Candidate.Owner)
	require.NoError(err)
	oldToken := publication.CurrentVectorToken

	next := work[0]
	next.Document.Revision = "synthetic-successor-revision"
	claim, acquired, err := f.Store.ClaimVisualWork(t.Context(), store.VisualClaimRequest{
		GenerationID: generation.ID, Owner: next.Candidate.Owner,
		ProposedRevision: next.Document.Revision, LeaseOwner: "successor",
		Now: time.Now().UTC(), LeaseDuration: time.Minute,
		SourceFence: next.Claim.SourceFence,
	})
	require.NoError(err)
	require.True(acquired)
	next.Claim = claim
	backend.deleteErr = errors.New("synthetic cleanup failure")

	second, err := worker.Run(t.Context(), []WorkItem{next})
	require.NoError(err)
	assert.Equal(int64(1), second.Published)
	assert.Equal(int64(1), second.CleanupFailures)
	publication, err = f.Store.GetVisualPublication(t.Context(), generation.ID, work[0].Candidate.Owner)
	require.NoError(err)
	assert.Equal(store.VisualPublicationCurrent, publication.State)
	assert.NotEqual(oldToken, publication.CurrentVectorToken)
}
