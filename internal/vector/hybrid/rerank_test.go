//go:build sqlite_vec

package hybrid

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/rerank"
)

// scriptedReranker stands in for a provider behind the rerank.Reranker
// contract. The HTTP contract itself is covered in package rerank.
type scriptedReranker struct {
	requests []rerank.Request
	scores   map[string]float64
	err      error
	onCall   func()
}

func (s *scriptedReranker) Rerank(ctx context.Context, request rerank.Request) (rerank.Result, error) {
	s.requests = append(s.requests, request)
	if s.onCall != nil {
		s.onCall()
	}
	if s.err != nil {
		return rerank.Result{}, s.err
	}
	if err := ctx.Err(); err != nil {
		return rerank.Result{}, err
	}
	scores := make([]float64, len(request.Candidates))
	for i, candidate := range request.Candidates {
		scores[i] = s.scores[candidate]
	}
	return rerank.Result{Scores: scores, Usage: rerank.Usage{Requests: 1}}, nil
}

var fixtureTexts = map[int64]string{1: "meeting tomorrow", 2: "lunch plans", 3: "travel itinerary"}

func textsFrom(texts map[int64]string) CandidateTexts {
	return func(_ context.Context, ids []int64) ([]string, error) {
		out := make([]string, len(ids))
		for i, id := range ids {
			out[i] = texts[id]
		}
		return out, nil
	}
}

func withRerank(f *engineFixture, reranker rerank.Reranker, texts CandidateTexts, candidates int) {
	f.Engine.cfg.Rerank = &RerankStage{
		Reranker: reranker, Texts: texts, Model: "test/reranker", Candidates: candidates,
	}
}

func hitIDs(hits []vector.FusedHit) []int64 {
	ids := make([]int64, len(hits))
	for i, hit := range hits {
		ids[i] = hit.MessageID
	}
	return ids
}

// baselineOrder runs the same search without reranking so assertions do
// not depend on how the backend breaks score ties.
func baselineOrder(t *testing.T, f *engineFixture) []int64 {
	t.Helper()
	hits, _, err := f.Engine.Search(context.Background(), SearchRequest{
		Mode: ModeHybrid, FreeText: "meeting", Limit: 3,
	})
	require.NoError(t, err)
	return hitIDs(hits)
}

func TestEngineRerankReordersByProviderScore(t *testing.T) {
	f := newEngineFixture(t)
	baseline := baselineOrder(t, f)
	require.Len(t, baseline, 3)
	reranker := &scriptedReranker{scores: map[string]float64{
		"travel itinerary": 0.9, "lunch plans": 0.5, "meeting tomorrow": 0.1,
	}}
	withRerank(f, reranker, textsFrom(fixtureTexts), 10)

	hits, meta, err := f.Engine.Search(context.Background(), SearchRequest{
		Mode: ModeHybrid, FreeText: "meeting", Limit: 3, Rerank: true,
	})
	require.NoError(t, err)
	assert.Equal(t, []int64{3, 2, 1}, hitIDs(hits))
	require.NotNil(t, hits[0].RerankScore)
	assert.InDelta(t, 0.9, *hits[0].RerankScore, 1e-9)
	assert.True(t, meta.Rerank.Requested)
	assert.True(t, meta.Rerank.Applied)
	assert.Equal(t, "test/reranker", meta.Rerank.Model)
	assert.Equal(t, 3, meta.Rerank.Candidates)
	assert.Empty(t, meta.Rerank.Fallback)
	require.Len(t, reranker.requests, 1)
	assert.Equal(t, "meeting", reranker.requests[0].Query)
	wantCandidates := make([]string, len(baseline))
	for i, id := range baseline {
		wantCandidates[i] = fixtureTexts[id]
	}
	assert.Equal(t, wantCandidates, reranker.requests[0].Candidates, "candidates are sent in retrieval order")
}

func TestEngineRerankWidensRetrievalToWindowAndTrimsToLimit(t *testing.T) {
	f := newEngineFixture(t)
	reranker := &scriptedReranker{scores: map[string]float64{
		"travel itinerary": 0.9, "lunch plans": 0.5, "meeting tomorrow": 0.1,
	}}
	withRerank(f, reranker, textsFrom(fixtureTexts), 3)

	hits, meta, err := f.Engine.Search(context.Background(), SearchRequest{
		Mode: ModeHybrid, FreeText: "meeting", Limit: 1, Rerank: true,
	})
	require.NoError(t, err)
	assert.Equal(t, []int64{3}, hitIDs(hits), "the reranked winner comes from outside the requested page")
	require.Len(t, reranker.requests, 1)
	assert.Len(t, reranker.requests[0].Candidates, 3)
	assert.True(t, meta.PoolSaturated, "hits beyond the page exist")
	assert.Equal(t, 1, meta.ReturnedCount)
}

func TestEngineRerankKeepsHitsOutsideWindowInRetrievalOrder(t *testing.T) {
	f := newEngineFixture(t)
	baseline := baselineOrder(t, f)
	reranker := &scriptedReranker{scores: map[string]float64{
		fixtureTexts[baseline[0]]: 0.2, fixtureTexts[baseline[1]]: 0.8,
	}}
	withRerank(f, reranker, textsFrom(fixtureTexts), 2)

	hits, _, err := f.Engine.Search(context.Background(), SearchRequest{
		Mode: ModeHybrid, FreeText: "meeting", Limit: 3, Rerank: true,
	})
	require.NoError(t, err)
	assert.Equal(t, []int64{baseline[1], baseline[0], baseline[2]}, hitIDs(hits))
	assert.Nil(t, hits[2].RerankScore, "a hit outside the window is not rescored")
}

func TestEngineRerankSkipsCandidatesWithoutText(t *testing.T) {
	f := newEngineFixture(t)
	baseline := baselineOrder(t, f)
	texts := map[int64]string{baseline[0]: "first", baseline[2]: "third"}
	reranker := &scriptedReranker{scores: map[string]float64{"first": 0.1, "third": 0.7}}
	withRerank(f, reranker, textsFrom(texts), 10)

	hits, meta, err := f.Engine.Search(context.Background(), SearchRequest{
		Mode: ModeHybrid, FreeText: "meeting", Limit: 3, Rerank: true,
	})
	require.NoError(t, err)
	require.Len(t, reranker.requests, 1)
	assert.Equal(t, []string{"first", "third"}, reranker.requests[0].Candidates)
	assert.Equal(t, []int64{baseline[2], baseline[0], baseline[1]}, hitIDs(hits))
	assert.Equal(t, 2, meta.Rerank.Candidates)
}

func TestEngineRerankFallsBackToRetrievalOrder(t *testing.T) {
	providerErr := &rerank.Error{Category: rerank.FailureProviderStatus, Status: 502}
	tests := []struct {
		name     string
		reranker *scriptedReranker
		texts    CandidateTexts
		fallback string
	}{
		{
			name:     "provider error",
			reranker: &scriptedReranker{err: providerErr},
			texts:    textsFrom(fixtureTexts),
			fallback: rerank.FailureProviderStatus,
		},
		{
			name:     "provider timeout",
			reranker: &scriptedReranker{err: &rerank.Error{Category: rerank.FailureTimeout}},
			texts:    textsFrom(fixtureTexts),
			fallback: rerank.FailureTimeout,
		},
		{
			name:     "score outside probability range",
			reranker: &scriptedReranker{scores: map[string]float64{"meeting tomorrow": 1.5}},
			texts:    textsFrom(fixtureTexts),
			fallback: rerank.FailureInvalidResponse,
		},
		{
			name:     "candidate text load failure",
			reranker: &scriptedReranker{},
			texts: func(context.Context, []int64) ([]string, error) {
				return nil, errors.New("database is locked")
			},
			fallback: FailureCandidateText,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newEngineFixture(t)
			baseline := baselineOrder(t, f)
			withRerank(f, tt.reranker, tt.texts, 10)

			hits, meta, err := f.Engine.Search(context.Background(), SearchRequest{
				Mode: ModeHybrid, FreeText: "meeting", Limit: 3, Rerank: true,
			})
			require.NoError(t, err, "a reranker failure must not fail the search")
			assert.Equal(t, baseline, hitIDs(hits))
			for _, hit := range hits {
				assert.Nil(t, hit.RerankScore)
			}
			assert.True(t, meta.Rerank.Requested)
			assert.False(t, meta.Rerank.Applied)
			assert.Equal(t, tt.fallback, meta.Rerank.Fallback)
			assert.Error(t, meta.Rerank.Err)
		})
	}
}

func TestEngineRerankReturnsCallerCancellation(t *testing.T) {
	f := newEngineFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	withRerank(f, &scriptedReranker{onCall: cancel}, textsFrom(fixtureTexts), 10)

	_, _, err := f.Engine.Search(ctx, SearchRequest{
		Mode: ModeHybrid, FreeText: "meeting", Limit: 3, Rerank: true,
	})
	require.ErrorIs(t, err, context.Canceled)
}

func TestEngineRerankRequiresStage(t *testing.T) {
	f := newEngineFixture(t)
	assert.False(t, f.Engine.RerankAvailable())

	_, _, err := f.Engine.Search(context.Background(), SearchRequest{
		Mode: ModeHybrid, FreeText: "meeting", Limit: 3, Rerank: true,
	})
	require.ErrorIs(t, err, ErrRerankNotConfigured)

	hits, meta, err := f.Engine.Search(context.Background(), SearchRequest{
		Mode: ModeHybrid, FreeText: "meeting", Limit: 3,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, hits)
	assert.False(t, meta.Rerank.Requested)
}
