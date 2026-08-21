package visual

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type searchProvider struct{ queries int }

func (*searchProvider) EmbedDocuments(context.Context, []DocumentInput) ([]EmbeddingResult, error) {
	return nil, nil
}
func (p *searchProvider) EmbedQuery(context.Context, QueryInput) ([]float32, Usage, error) {
	p.queries++
	return make([]float32, 1024), Usage{Available: true}, nil
}

type searchBackend struct {
	hits     []Hit
	requests []SearchRequest
}

func (*searchBackend) PutUnpublished(context.Context, VectorToken, []float32) error { return nil }
func (*searchBackend) DeleteTokens(context.Context, []VectorToken) error            { return nil }
func (b *searchBackend) Search(_ context.Context, request SearchRequest) ([]Hit, error) {
	b.requests = append(b.requests, request)
	result := make([]Hit, 0, len(b.hits))
	for _, hit := range b.hits {
		if request.AfterScore != nil && (hit.Score > *request.AfterScore ||
			(hit.Score == *request.AfterScore && hit.Token <= request.AfterToken)) {
			continue
		}
		result = append(result, hit)
		if len(result) == request.Limit {
			break
		}
	}
	return result, nil
}
func (*searchBackend) LoadOwnerVector(context.Context, GenerationID, Owner) ([]float32, error) {
	return nil, nil
}

func TestSearchServiceRequiresActiveGenerationAndExclusiveQueryMode(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := storetest.New(t)
	provider := &searchProvider{}
	service, err := NewSearchService(f.Store, provider, &searchBackend{}, true)
	requirements.NoError(err)

	_, err = service.Search(t.Context(), SearchQuery{Text: "diagram", Image: &MediaInput{}})
	requirements.ErrorIs(err, ErrInvalidQuery)
	assertions.Zero(provider.queries)

	_, err = service.Search(t.Context(), SearchQuery{Text: "diagram"})
	requirements.ErrorIs(err, ErrSearchNotReady)
	assertions.Zero(provider.queries)

	generation, err := f.Store.EnsureVisualGeneration(t.Context(), store.VisualGenerationSpec{
		Fingerprint: "search-test", Model: "voyage-multimodal-3.5", Dimension: 1024,
	})
	requirements.NoError(err)
	requirements.NoError(f.Store.ConsentVisualGeneration(t.Context(), generation.ID, "synthetic-policy-fingerprint"))
	_, activateStoreErr := f.Store.ActivateVisualGeneration(t.Context(), generation.ID, 0)
	requirements.NoError(activateStoreErr)

	response, err := service.Search(t.Context(), SearchQuery{Text: "diagram", Limit: 5})
	requirements.NoError(err)
	assertions.Equal(generation.ID, response.GenerationID)
	assertions.Equal("text", response.QueryMode)
	assertions.Empty(response.Results)
	assertions.Equal(1, provider.queries)
}

func TestSearchCursorSurvivesDeletionAndRejectsQueryMismatch(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := storetest.New(t)
	generation := testVisualGeneration(t, f)
	firstMessage := testVisualCandidate(t, f, "search-page-first", strings.Repeat("91", 32))
	secondMessage := testVisualCandidate(t, f, "search-page-second", strings.Repeat("92", 32))
	reconciler := testReconciler(t, f, generation.ID, memoryOpener{data: encodedPNG(t, 2, 2)}, "visual-test/search-page")
	for {
		result, err := reconciler.FullReconcile(t.Context())
		requirements.NoError(err)
		if len(result.Work) == 0 {
			break
		}
		for _, work := range result.Work {
			publishReconciledWork(t, f, work)
		}
	}
	requirements.NoError(f.Store.ConsentVisualGeneration(t.Context(), generation.ID, "synthetic-policy-fingerprint"))
	retired, activateErr := reconciler.Activate(t.Context())
	requirements.NoError(activateErr)
	requirements.Empty(retired)
	firstPublication := visualPublicationForMessage(t, f, generation.ID, firstMessage)
	secondPublication := visualPublicationForMessage(t, f, generation.ID, secondMessage)
	backend := &searchBackend{hits: []Hit{
		{Token: VectorToken(firstPublication.CurrentVectorToken), Score: .9, Rank: 1},
		{Token: VectorToken(secondPublication.CurrentVectorToken), Score: .8, Rank: 2},
	}}
	provider := &searchProvider{}
	service, err := NewSearchService(f.Store, provider, backend, true)
	requirements.NoError(err)

	first, err := service.Search(t.Context(), SearchQuery{Text: "diagram", Limit: 1})
	requirements.NoError(err)
	requirements.Len(first.Results, 1)
	requirements.NotEmpty(first.NextCursor)
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
		`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ? RETURNING id`), firstMessage).Scan(&firstMessage))

	second, err := service.Search(t.Context(), SearchQuery{Text: "diagram", Limit: 1, Cursor: first.NextCursor})
	requirements.NoError(err)
	requirements.Len(second.Results, 1)
	assertions.Equal(secondMessage, second.Results[0].MessageID)
	queriesBeforeMismatch := provider.queries
	_, err = service.Search(t.Context(), SearchQuery{Text: "different", Limit: 1, Cursor: first.NextCursor})
	requirements.ErrorIs(err, ErrInvalidCursor)
	assertions.Equal(queriesBeforeMismatch, provider.queries)
}

func TestDecodeQueryImageRejectsNonImage(t *testing.T) {
	_, err := DecodeQueryImage([]byte("not an image"))
	require.Error(t, err)
}

func TestSearchRejectsDriftedAccountScopeBeforeProviderOrBackend(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	provider := &searchProvider{}
	backend := &searchBackend{}
	service, err := NewSearchService(f.Store, provider, backend, true)
	require.NoError(err)
	// The account-to-source mapping changed after initialization: the scope
	// preflight fails, and search must report not-ready — the text lane
	// latches searches stale under the same drift — without paying a
	// provider embedding or touching the backend.
	service.SetScopeCheck(func(context.Context) error {
		return errors.New("the multimodal account scope no longer resolves to the same sources")
	})

	_, err = service.Search(t.Context(), SearchQuery{Text: "diagram"})
	require.ErrorIs(err, ErrSearchNotReady)
	_, _, err = service.EmbedQueryVector(t.Context(), SearchQuery{Text: "diagram"})
	require.ErrorIs(err, ErrSearchNotReady)
	assert.Zero(provider.queries, "no hosted embedding on drift")
	assert.Empty(backend.requests, "no backend search on drift")
}
