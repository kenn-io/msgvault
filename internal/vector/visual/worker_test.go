package visual

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type fakeVisualProvider struct {
	mu       sync.Mutex
	calls    int
	callback func([]DocumentInput)
	embed    func(context.Context, []DocumentInput) ([]EmbeddingResult, error)
}

func (p *fakeVisualProvider) EmbedDocuments(
	ctx context.Context,
	documents []DocumentInput,
) ([]EmbeddingResult, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	if p.callback != nil {
		p.callback(documents)
	}
	if p.embed != nil {
		return p.embed(ctx, documents)
	}
	results := make([]EmbeddingResult, len(documents))
	for index := range documents {
		results[index] = EmbeddingResult{
			Owner: documents[index].Owner, Vector: []float32{float32(index + 1), 2},
		}
	}
	return results, nil
}

func (p *fakeVisualProvider) EmbedQuery(context.Context, QueryInput) ([]float32, Usage, error) {
	return nil, Usage{}, errors.New("not implemented")
}

func (p *fakeVisualProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type fakeVisualBackend struct {
	mu             sync.Mutex
	vectors        map[VectorToken][]float32
	deleted        []VectorToken
	putErr         error
	deleteErr      error
	putPublication func(VectorToken)
}

func newFakeVisualBackend() *fakeVisualBackend {
	return &fakeVisualBackend{vectors: make(map[VectorToken][]float32)}
}

func (b *fakeVisualBackend) PutUnpublished(
	_ context.Context,
	token VectorToken,
	vector []float32,
) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.putErr != nil {
		return b.putErr
	}
	b.vectors[token] = append([]float32(nil), vector...)
	if b.putPublication != nil {
		b.putPublication(token)
	}
	return nil
}

func (b *fakeVisualBackend) DeleteTokens(_ context.Context, tokens []VectorToken) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.deleteErr != nil {
		return b.deleteErr
	}
	for _, token := range tokens {
		delete(b.vectors, token)
		b.deleted = append(b.deleted, tokens...)
	}
	return nil
}

func (b *fakeVisualBackend) Search(context.Context, SearchRequest) ([]Hit, error) {
	return nil, nil
}

func (b *fakeVisualBackend) LoadOwnerVector(context.Context, GenerationID, Owner) ([]float32, error) {
	return nil, errors.New("not implemented")
}

func TestVisualWorkerPublishesOnlyAfterUnreachableVectorWrite(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, generation, work := workerFixture(t, 1)
	provider := &fakeVisualProvider{}
	backend := newFakeVisualBackend()
	backend.putPublication = func(token VectorToken) {
		publication, err := f.Store.GetVisualPublication(
			t.Context(), generation.ID, work[0].Candidate.Owner,
		)
		require.NoError(err)
		assert.Equal(store.VisualPublicationStale, publication.State)
		assert.Empty(publication.CurrentVectorToken)
		assert.Equal(string(token), publication.PendingVectorToken)
	}
	worker := newTestVisualWorker(t, f, provider, backend)

	result, err := worker.Run(t.Context(), work)
	require.NoError(err)
	assert.Equal(int64(1), result.ProviderRequests)
	assert.Equal(int64(1), result.Published)
	assert.Equal(int64(1), result.Attempted)
	assert.Equal(int64(1), result.Succeeded)
	assert.Zero(result.Failed)
	assert.Zero(result.Skipped)
	publication, err := f.Store.GetVisualPublication(t.Context(), generation.ID, work[0].Candidate.Owner)
	require.NoError(err)
	assert.Equal(store.VisualPublicationCurrent, publication.State)
	assert.Equal(work[0].Document.Revision, publication.PublishedRevision)
	assert.NotEmpty(publication.CurrentVectorToken)
	assert.Equal([]float32{1, 2}, backend.vectors[VectorToken(publication.CurrentVectorToken)])
}

func TestVisualWorkerSplitsSizeRejectedBatchesWithoutTruncatingMedia(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, _, work := workerFixture(t, 2)
	provider := &fakeVisualProvider{embed: func(_ context.Context, documents []DocumentInput) ([]EmbeddingResult, error) {
		if len(documents) > 1 {
			return nil, ErrProviderBatchTooLarge
		}
		return []EmbeddingResult{{Owner: documents[0].Owner, Vector: []float32{1, 2}}}, nil
	}}
	backend := newFakeVisualBackend()
	worker := newTestVisualWorker(t, f, provider, backend)

	result, err := worker.Run(t.Context(), work)
	require.NoError(err)
	assert.Equal(int64(3), result.ProviderRequests)
	assert.Equal(int64(2), result.Published)
	assert.Equal(3, provider.callCount())
}

func TestVisualWorkerRecordsTerminalSingletonSizeAndRetryableMalformedResponse(t *testing.T) {
	t.Run("terminal singleton", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		f, generation, work := workerFixture(t, 1)
		provider := &fakeVisualProvider{embed: func(context.Context, []DocumentInput) ([]EmbeddingResult, error) {
			return nil, ErrProviderBatchTooLarge
		}}
		worker := newTestVisualWorker(t, f, provider, newFakeVisualBackend())

		result, err := worker.Run(t.Context(), work)
		require.NoError(err)
		assert.Equal(int64(1), result.Terminal)
		publication, err := f.Store.GetVisualPublication(t.Context(), generation.ID, work[0].Candidate.Owner)
		require.NoError(err)
		assert.Equal("terminal", publication.OutcomeKind)
		assert.Equal(string(ReasonProviderLimit), publication.OutcomeReason)
	})

	t.Run("retryable malformed", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		f, generation, work := workerFixture(t, 1)
		provider := &fakeVisualProvider{embed: func(context.Context, []DocumentInput) ([]EmbeddingResult, error) {
			return []EmbeddingResult{{Owner: work[0].Document.Owner, Vector: []float32{1}}}, nil
		}}
		worker := newTestVisualWorker(t, f, provider, newFakeVisualBackend())

		result, err := worker.Run(t.Context(), work)
		require.NoError(err)
		assert.Equal(int64(1), result.Retryable)
		publication, err := f.Store.GetVisualPublication(t.Context(), generation.ID, work[0].Candidate.Owner)
		require.NoError(err)
		assert.Equal("retryable", publication.OutcomeKind)
		assert.Equal("provider_malformed_response", publication.OutcomeReason)
	})
}

func TestVisualWorkerDiscardsLateResultAfterSourceChange(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, generation, work := workerFixture(t, 1)
	provider := &fakeVisualProvider{callback: func([]DocumentInput) {
		_, err := f.Store.DB().Exec(f.Store.Rebind(
			`UPDATE messages SET subject = ? WHERE id = ?`),
			"changed during provider request", work[0].Candidate.Owner.MessageID)
		require.NoError(err)
	}}
	backend := newFakeVisualBackend()
	worker := newTestVisualWorker(t, f, provider, backend)

	result, err := worker.Run(t.Context(), work)
	require.NoError(err)
	assert.Equal(int64(1), result.Obsolete)
	assert.Zero(result.Published)
	assert.Equal(int64(1), result.Attempted)
	assert.Zero(result.Succeeded)
	assert.Zero(result.Failed)
	assert.Equal(int64(1), result.Skipped)
	assert.Equal(result.Attempted, result.Succeeded+result.Failed+result.Skipped)
	assert.Empty(backend.vectors)
	publication, err := f.Store.GetVisualPublication(t.Context(), generation.ID, work[0].Candidate.Owner)
	require.NoError(err)
	assert.NotEqual(store.VisualPublicationCurrent, publication.State)
	assert.Empty(publication.CurrentVectorToken)
}

func TestVisualWorkerVectorWriteFailureLeavesNothingVisibleAndReleasesClaim(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, generation, work := workerFixture(t, 1)
	backend := newFakeVisualBackend()
	backend.putErr = errors.New("synthetic vector disk failure")
	worker := newTestVisualWorker(t, f, &fakeVisualProvider{}, backend)

	_, err := worker.Run(t.Context(), work)
	require.ErrorContains(err, "synthetic vector disk failure")
	publication, err := f.Store.GetVisualPublication(t.Context(), generation.ID, work[0].Candidate.Owner)
	require.NoError(err)
	assert.Equal(store.VisualPublicationStale, publication.State)
	assert.Empty(publication.CurrentVectorToken)
	assert.Empty(backend.vectors)

	_, acquired, err := f.Store.ClaimVisualWork(t.Context(), store.VisualClaimRequest{
		GenerationID: generation.ID, Owner: work[0].Candidate.Owner,
		ProposedRevision: work[0].Document.Revision, LeaseOwner: "successor",
		Now: time.Now().UTC(), LeaseDuration: time.Minute, SourceFence: work[0].Claim.SourceFence,
	})
	require.NoError(err)
	assert.True(acquired, "released failed work must be immediately reclaimable")
}

func TestVisualWorkerRejectsObsoleteFenceAndNonFiniteProviderVector(t *testing.T) {
	t.Run("obsolete fence", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		f, _, work := workerFixture(t, 1)
		request := store.VisualClaimRequest{
			GenerationID: work[0].Claim.GenerationID, Owner: work[0].Candidate.Owner,
			ProposedRevision: work[0].Document.Revision, LeaseOwner: "successor",
			Now: work[0].Claim.LeaseExpiresAt.Add(time.Second), LeaseDuration: time.Minute,
			SourceFence: work[0].Claim.SourceFence,
		}
		_, acquired, err := f.Store.ClaimVisualWork(t.Context(), request)
		require.NoError(err)
		require.True(acquired)
		worker := newTestVisualWorker(t, f, &fakeVisualProvider{}, newFakeVisualBackend())

		result, err := worker.Run(t.Context(), work)
		require.NoError(err)
		assert.Equal(int64(1), result.Obsolete)
		assert.Zero(result.Published)
	})

	t.Run("non finite", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		f, generation, work := workerFixture(t, 1)
		provider := &fakeVisualProvider{embed: func(context.Context, []DocumentInput) ([]EmbeddingResult, error) {
			return []EmbeddingResult{{Owner: work[0].Document.Owner, Vector: []float32{1, float32(math.Inf(1))}}}, nil
		}}
		worker := newTestVisualWorker(t, f, provider, newFakeVisualBackend())
		result, err := worker.Run(t.Context(), work)
		require.NoError(err)
		assert.Equal(int64(1), result.Retryable)
		publication, err := f.Store.GetVisualPublication(t.Context(), generation.ID, work[0].Candidate.Owner)
		require.NoError(err)
		assert.Equal("provider_malformed_response", publication.OutcomeReason)
	})
}

func TestVisualWorkerRenewsClaimsDuringProviderRequestAndCancelsCleanly(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, _, work := workerFixture(t, 1)
	provider := &fakeVisualProvider{embed: func(ctx context.Context, documents []DocumentInput) ([]EmbeddingResult, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(30 * time.Millisecond):
		}
		return []EmbeddingResult{{Owner: documents[0].Owner, Vector: []float32{1, 2}}}, nil
	}}
	worker, err := NewWorker(f.Store, provider, newFakeVisualBackend(), WorkerConfig{
		Dimension: 2, ProviderTimeout: 50 * time.Millisecond,
		LeaseDuration: 100 * time.Millisecond, RenewInterval: 5 * time.Millisecond,
	})
	require.NoError(err)

	result, err := worker.Run(t.Context(), work)
	require.NoError(err)
	assert.Equal(int64(1), result.Published)

	f, _, work = workerFixture(t, 1)
	provider = &fakeVisualProvider{embed: func(ctx context.Context, _ []DocumentInput) ([]EmbeddingResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	worker = newTestVisualWorker(t, f, provider, newFakeVisualBackend())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = worker.Run(ctx, work)
	require.ErrorIs(err, context.Canceled)
	_, acquired, err := f.Store.ClaimVisualWork(t.Context(), store.VisualClaimRequest{
		GenerationID: work[0].Claim.GenerationID, Owner: work[0].Candidate.Owner,
		ProposedRevision: work[0].Document.Revision, LeaseOwner: "successor",
		Now: time.Now().UTC(), LeaseDuration: time.Minute, SourceFence: work[0].Claim.SourceFence,
	})
	require.NoError(err)
	assert.True(acquired, "canceled work must be immediately reclaimable")
}

func workerFixture(
	t *testing.T,
	count int,
) (*storetest.Fixture, store.VisualGeneration, []WorkItem) {
	t.Helper()
	f := storetest.New(t)
	generation := testVisualGeneration(t, f)
	for index := range count {
		hash := strings.Repeat(string(rune('a'+index)), 64)
		testVisualCandidate(t, f, "worker-message-"+string(rune('a'+index)), hash)
	}
	reconciler := testReconciler(t, f, generation.ID,
		memoryOpener{data: encodedPNG(t, 2, 2)}, "visual-test/worker-"+string(rune('a'+count)))
	result, err := reconciler.FullReconcile(t.Context())
	require.NoError(t, err)
	require.Len(t, result.Work, count)
	return f, generation, result.Work
}

func newTestVisualWorker(
	t *testing.T,
	f *storetest.Fixture,
	provider Provider,
	backend Backend,
) *Worker {
	t.Helper()
	worker, err := NewWorker(f.Store, provider, backend, WorkerConfig{
		Dimension: 2, ProviderTimeout: time.Second,
		LeaseDuration: 2 * time.Second, RenewInterval: 100 * time.Millisecond,
		MaxBatchItems: 10,
	})
	require.NoError(t, err)
	return worker
}

var _ Provider = (*fakeVisualProvider)(nil)
var _ Backend = (*fakeVisualBackend)(nil)

func TestVisualWorkerBisectsRejectedBatchesSoValidNeighborsPublish(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, _, work := workerFixture(t, 2)
	// The provider rejects any batch containing the first owner; the second
	// document is valid. A whole-batch terminal outcome would permanently
	// exclude it.
	badOwner := work[0].Candidate.Owner
	provider := &fakeVisualProvider{embed: func(_ context.Context, documents []DocumentInput) ([]EmbeddingResult, error) {
		for _, document := range documents {
			if document.Owner.MessageID == badOwner.MessageID {
				return nil, ErrProviderRejected
			}
		}
		results := make([]EmbeddingResult, len(documents))
		for index := range documents {
			results[index] = EmbeddingResult{Owner: documents[index].Owner, Vector: []float32{1, 2}}
		}
		return results, nil
	}}
	backend := newFakeVisualBackend()
	worker := newTestVisualWorker(t, f, provider, backend)

	result, err := worker.Run(t.Context(), work)
	require.NoError(err)
	assert.Equal(int64(1), result.Published, "the valid neighbor publishes")
	assert.Equal(int64(1), result.Terminal, "only the rejected singleton is terminal")
	assert.Equal(int64(2), result.Attempted,
		"provider bisection requests must not multiply final attachment outcomes")
	assert.Equal(int64(1), result.Succeeded)
	assert.Equal(int64(1), result.Failed)
	assert.Zero(result.Skipped)
	publication, err := f.Store.GetVisualPublication(t.Context(), work[0].Claim.GenerationID, badOwner)
	require.NoError(err)
	assert.Equal(OutcomeTerminal, publication.OutcomeKind)
}
