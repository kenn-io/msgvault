package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/googlereviews"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const (
	googleReviewTestTarget   = "shop"
	googleReviewTestLocation = "accounts/a/locations/l"
)

func syntheticGoogleReview(id, comment string) googlereviews.Review {
	return googlereviews.Review{
		Name: googleReviewTestLocation + "/reviews/" + id, ReviewID: id,
		Reviewer:   googlereviews.Reviewer{DisplayName: "Example Reviewer"},
		StarRating: "FIVE", Comment: comment,
		CreateTime: "2025-01-02T03:04:05Z", UpdateTime: "2025-01-02T03:04:05Z",
	}
}

func TestGoogleReviewsArchiveResumesUpsertsAndSearches(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()

	state, err := st.BeginGoogleReviewsBackfill(ctx, googleReviewTestTarget, googleReviewTestLocation)
	require.NoError(err)
	assert.Equal("running", state.Status)
	assert.Equal(int64(1), state.RunID)
	assert.Empty(state.NextPageToken)

	pageOne := googlereviews.Page{
		Reviews:          []googlereviews.Review{syntheticGoogleReview("r1", "synthetic needle in the first review")},
		NextPageToken:    "page-two",
		TotalReviewCount: 2,
	}
	state, err = st.ApplyGoogleReviewsPage(ctx, googleReviewTestTarget, googleReviewTestLocation, "", pageOne)
	require.NoError(err)
	assert.Equal("running", state.Status)
	assert.Equal(int64(1), state.PagesFetched)
	assert.Equal("page-two", state.NextPageToken)
	assert.Equal(int64(1), state.ArchivedReviewCount)

	state, err = st.FailGoogleReviewsBackfill(ctx, googleReviewTestTarget, googleReviewTestLocation, "quota")
	require.NoError(err)
	assert.Equal("partial", state.Status)
	assert.Equal("quota", state.LastErrorCode)

	state, err = st.BeginGoogleReviewsBackfill(ctx, googleReviewTestTarget, googleReviewTestLocation)
	require.NoError(err)
	assert.Equal(int64(1), state.RunID)
	assert.Equal(int64(1), state.PagesFetched)
	assert.Equal("page-two", state.NextPageToken)

	pageTwo := googlereviews.Page{
		Reviews:          []googlereviews.Review{syntheticGoogleReview("r2", "second synthetic review")},
		TotalReviewCount: 2,
	}
	state, err = st.ApplyGoogleReviewsPage(ctx, googleReviewTestTarget, googleReviewTestLocation, "page-two", pageTwo)
	require.NoError(err)
	assert.Equal("api_exhausted", state.Status)
	assert.Empty(state.NextPageToken)
	assert.Equal(int64(2), state.PagesFetched)
	assert.Equal(int64(2), state.ReviewsFetched)
	assert.Equal(int64(2), state.ArchivedReviewCount)
	assert.Equal(int64(2), state.TotalReviewCount)
	require.NotNil(state.CompletedAt)

	search, err := st.SearchGoogleReviews(ctx, "needle", "", 0, 20)
	require.NoError(err)
	assert.Equal(int64(1), search.Total)
	require.Len(search.Results, 1)
	assert.Equal("r1", search.Results[0].Review.ReviewID)
	assert.False(search.Results[0].ArchivedAt.IsZero())

	state, err = st.BeginGoogleReviewsBackfill(ctx, googleReviewTestTarget, googleReviewTestLocation)
	require.NoError(err)
	assert.Equal(int64(2), state.RunID)
	assert.Zero(state.PagesFetched)
	assert.Empty(state.NextPageToken)

	// A later traversal that omits an older result never deletes it.
	state, err = st.ApplyGoogleReviewsPage(ctx, googleReviewTestTarget, googleReviewTestLocation, "", googlereviews.Page{
		Reviews: []googlereviews.Review{syntheticGoogleReview("r1", "updated synthetic needle")},
	})
	require.NoError(err)
	assert.Equal(int64(2), state.ArchivedReviewCount)
	search, err = st.SearchGoogleReviews(ctx, "second", googleReviewTestTarget, 0, 20)
	require.NoError(err)
	assert.Equal(int64(1), search.Total)
	assert.Equal("r2", search.Results[0].Review.ReviewID)
}

func TestGoogleReviewsArchiveRejectsRepeatedPageTokenAtomically(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	_, err := st.BeginGoogleReviewsBackfill(ctx, googleReviewTestTarget, googleReviewTestLocation)
	require.NoError(err)
	_, err = st.ApplyGoogleReviewsPage(ctx, googleReviewTestTarget, googleReviewTestLocation, "", googlereviews.Page{
		Reviews: []googlereviews.Review{syntheticGoogleReview("r1", "first page")}, NextPageToken: "repeat",
	})
	require.NoError(err)

	_, err = st.ApplyGoogleReviewsPage(ctx, googleReviewTestTarget, googleReviewTestLocation, "repeat", googlereviews.Page{
		Reviews: []googlereviews.Review{syntheticGoogleReview("r2", "must roll back")}, NextPageToken: "repeat",
	})
	require.ErrorIs(err, googlereviews.ErrArchiveCycle)

	state, err := st.GoogleReviewsBackfillState(ctx, googleReviewTestTarget, googleReviewTestLocation)
	require.NoError(err)
	assert.Equal("running", state.Status)
	assert.Equal(int64(1), state.PagesFetched)
	assert.Equal("repeat", state.NextPageToken)
	assert.Equal(int64(1), state.ArchivedReviewCount)

	state, err = st.FailGoogleReviewsBackfill(ctx, googleReviewTestTarget, googleReviewTestLocation, "cursor_cycle")
	require.NoError(err)
	assert.Equal("partial", state.Status)
	assert.Equal("cursor_cycle", state.LastErrorCode)
	state, err = st.BeginGoogleReviewsBackfill(ctx, googleReviewTestTarget, googleReviewTestLocation)
	require.NoError(err)
	assert.Equal(int64(2), state.RunID)
	assert.Equal("running", state.Status)
	assert.Zero(state.PagesFetched)
	assert.Zero(state.ReviewsFetched)
	assert.Empty(state.NextPageToken)

	state, err = st.ApplyGoogleReviewsPage(ctx, googleReviewTestTarget, googleReviewTestLocation, "", googlereviews.Page{
		Reviews: []googlereviews.Review{syntheticGoogleReview("r1", "first page retried")}, NextPageToken: "repeat",
	})
	require.NoError(err, "an explicit retry starts a fresh token traversal")
	assert.Equal(int64(2), state.RunID)
	assert.Equal(int64(1), state.PagesFetched)
	assert.Equal("repeat", state.NextPageToken)
	assert.Equal(int64(1), state.ArchivedReviewCount)
}

func TestGoogleReviewsArchiveSearchRejectsEmptyQuery(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.SearchGoogleReviews(t.Context(), " \t", "", 0, 20)
	require.ErrorIs(err, googlereviews.ErrSearch)
}

func TestGoogleReviewsSchemaUpgradeAddsArchiveAndSearch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	for _, table := range []string{"google_reviews_sync_tokens", "google_reviews_sync_state", "google_reviews"} {
		_, err := st.DB().ExecContext(ctx, "DROP TABLE "+table)
		require.NoError(err)
	}
	_, err := st.DB().ExecContext(ctx, "UPDATE archive_metadata SET value = '33' WHERE key = 'schema_version'")
	require.NoError(err)

	require.NoError(st.InitSchemaContext(ctx))
	version, err := st.SchemaVersionContext(ctx)
	require.NoError(err)
	assert.Equal(store.SchemaVersion, version)

	_, err = st.BeginGoogleReviewsBackfill(ctx, googleReviewTestTarget, googleReviewTestLocation)
	require.NoError(err)
	_, err = st.ApplyGoogleReviewsPage(ctx, googleReviewTestTarget, googleReviewTestLocation, "", googlereviews.Page{
		Reviews: []googlereviews.Review{syntheticGoogleReview("r-migrated", "migration search token")},
	})
	require.NoError(err)
	response, err := st.SearchGoogleReviews(ctx, "migration", googleReviewTestTarget, 0, 20)
	require.NoError(err)
	assert.Equal(int64(1), response.Total)
}

var _ googlereviews.Archive = (*store.Store)(nil)

func TestGoogleReviewsStatusUsesConfiguredLocation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	const other = "accounts/a/locations/other"
	_, err := st.BeginGoogleReviewsBackfill(ctx, googleReviewTestTarget, googleReviewTestLocation)
	require.NoError(err)
	_, err = st.BeginGoogleReviewsBackfill(ctx, googleReviewTestTarget, other)
	require.NoError(err)
	_, err = st.FailGoogleReviewsBackfill(ctx, googleReviewTestTarget, googleReviewTestLocation, "quota")
	require.NoError(err)
	state, err := st.GoogleReviewsBackfillState(ctx, googleReviewTestTarget, other)
	require.NoError(err)
	assert.Equal(other, state.Location)
	assert.Equal("running", state.Status)
	assert.Empty(state.LastErrorCode)
}

func TestGoogleReviewsArchiveSearchPaginatesAliasesWithSameReviewID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	page := googlereviews.Page{Reviews: []googlereviews.Review{syntheticGoogleReview("same", "synthetic matching text")}}
	for _, target := range []string{"shop", "alias"} {
		_, err := st.BeginGoogleReviewsBackfill(ctx, target, googleReviewTestLocation)
		require.NoError(err)
		_, err = st.ApplyGoogleReviewsPage(ctx, target, googleReviewTestLocation, "", page)
		require.NoError(err)
	}
	first, err := st.SearchGoogleReviews(ctx, "synthetic", "", 0, 1)
	require.NoError(err)
	second, err := st.SearchGoogleReviews(ctx, "synthetic", "", 1, 1)
	require.NoError(err)
	require.Len(first.Results, 1)
	require.Len(second.Results, 1)
	assert.NotEqual(first.Results[0].Target, second.Results[0].Target)
}
