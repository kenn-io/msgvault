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
	st := testutil.NewTestStore(t)
	ctx := t.Context()

	state, err := st.BeginGoogleReviewsBackfill(ctx, googleReviewTestTarget, googleReviewTestLocation)
	require.NoError(t, err)
	assert.Equal(t, "running", state.Status)
	assert.Equal(t, int64(1), state.RunID)
	assert.Empty(t, state.NextPageToken)

	pageOne := googlereviews.Page{
		Reviews:          []googlereviews.Review{syntheticGoogleReview("r1", "synthetic needle in the first review")},
		NextPageToken:    "page-two",
		TotalReviewCount: 2,
	}
	state, err = st.ApplyGoogleReviewsPage(ctx, googleReviewTestTarget, googleReviewTestLocation, "", pageOne)
	require.NoError(t, err)
	assert.Equal(t, "running", state.Status)
	assert.Equal(t, int64(1), state.PagesFetched)
	assert.Equal(t, "page-two", state.NextPageToken)
	assert.Equal(t, int64(1), state.ArchivedReviewCount)

	state, err = st.FailGoogleReviewsBackfill(ctx, googleReviewTestTarget, googleReviewTestLocation, "quota")
	require.NoError(t, err)
	assert.Equal(t, "partial", state.Status)
	assert.Equal(t, "quota", state.LastErrorCode)

	state, err = st.BeginGoogleReviewsBackfill(ctx, googleReviewTestTarget, googleReviewTestLocation)
	require.NoError(t, err)
	assert.Equal(t, int64(1), state.RunID)
	assert.Equal(t, int64(1), state.PagesFetched)
	assert.Equal(t, "page-two", state.NextPageToken)

	pageTwo := googlereviews.Page{
		Reviews:          []googlereviews.Review{syntheticGoogleReview("r2", "second synthetic review")},
		TotalReviewCount: 2,
	}
	state, err = st.ApplyGoogleReviewsPage(ctx, googleReviewTestTarget, googleReviewTestLocation, "page-two", pageTwo)
	require.NoError(t, err)
	assert.Equal(t, "api_exhausted", state.Status)
	assert.Empty(t, state.NextPageToken)
	assert.Equal(t, int64(2), state.PagesFetched)
	assert.Equal(t, int64(2), state.ReviewsFetched)
	assert.Equal(t, int64(2), state.ArchivedReviewCount)
	assert.Equal(t, int64(2), state.TotalReviewCount)
	require.NotNil(t, state.CompletedAt)

	search, err := st.SearchGoogleReviews(ctx, "needle", "", 0, 20)
	require.NoError(t, err)
	assert.Equal(t, int64(1), search.Total)
	require.Len(t, search.Results, 1)
	assert.Equal(t, "r1", search.Results[0].Review.ReviewID)
	assert.False(t, search.Results[0].ArchivedAt.IsZero())

	state, err = st.BeginGoogleReviewsBackfill(ctx, googleReviewTestTarget, googleReviewTestLocation)
	require.NoError(t, err)
	assert.Equal(t, int64(2), state.RunID)
	assert.Zero(t, state.PagesFetched)
	assert.Empty(t, state.NextPageToken)

	// A later traversal that omits an older result never deletes it.
	state, err = st.ApplyGoogleReviewsPage(ctx, googleReviewTestTarget, googleReviewTestLocation, "", googlereviews.Page{
		Reviews: []googlereviews.Review{syntheticGoogleReview("r1", "updated synthetic needle")},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), state.ArchivedReviewCount)
	search, err = st.SearchGoogleReviews(ctx, "second", googleReviewTestTarget, 0, 20)
	require.NoError(t, err)
	assert.Equal(t, int64(1), search.Total)
	assert.Equal(t, "r2", search.Results[0].Review.ReviewID)
}

func TestGoogleReviewsArchiveRejectsRepeatedPageTokenAtomically(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	_, err := st.BeginGoogleReviewsBackfill(ctx, googleReviewTestTarget, googleReviewTestLocation)
	require.NoError(t, err)
	_, err = st.ApplyGoogleReviewsPage(ctx, googleReviewTestTarget, googleReviewTestLocation, "", googlereviews.Page{
		Reviews: []googlereviews.Review{syntheticGoogleReview("r1", "first page")}, NextPageToken: "repeat",
	})
	require.NoError(t, err)

	_, err = st.ApplyGoogleReviewsPage(ctx, googleReviewTestTarget, googleReviewTestLocation, "repeat", googlereviews.Page{
		Reviews: []googlereviews.Review{syntheticGoogleReview("r2", "must roll back")}, NextPageToken: "repeat",
	})
	require.ErrorIs(t, err, googlereviews.ErrArchiveCycle)

	state, err := st.GoogleReviewsBackfillState(ctx, googleReviewTestTarget)
	require.NoError(t, err)
	assert.Equal(t, "running", state.Status)
	assert.Equal(t, int64(1), state.PagesFetched)
	assert.Equal(t, "repeat", state.NextPageToken)
	assert.Equal(t, int64(1), state.ArchivedReviewCount)

	state, err = st.FailGoogleReviewsBackfill(ctx, googleReviewTestTarget, googleReviewTestLocation, "cursor_cycle")
	require.NoError(t, err)
	assert.Equal(t, "partial", state.Status)
	assert.Equal(t, "cursor_cycle", state.LastErrorCode)
	state, err = st.BeginGoogleReviewsBackfill(ctx, googleReviewTestTarget, googleReviewTestLocation)
	require.NoError(t, err)
	assert.Equal(t, int64(2), state.RunID)
	assert.Equal(t, "running", state.Status)
	assert.Zero(t, state.PagesFetched)
	assert.Zero(t, state.ReviewsFetched)
	assert.Empty(t, state.NextPageToken)

	state, err = st.ApplyGoogleReviewsPage(ctx, googleReviewTestTarget, googleReviewTestLocation, "", googlereviews.Page{
		Reviews: []googlereviews.Review{syntheticGoogleReview("r1", "first page retried")}, NextPageToken: "repeat",
	})
	require.NoError(t, err, "an explicit retry starts a fresh token traversal")
	assert.Equal(t, int64(2), state.RunID)
	assert.Equal(t, int64(1), state.PagesFetched)
	assert.Equal(t, "repeat", state.NextPageToken)
	assert.Equal(t, int64(1), state.ArchivedReviewCount)
}

func TestGoogleReviewsArchiveSearchRejectsEmptyQuery(t *testing.T) {
	st := testutil.NewTestStore(t)
	_, err := st.SearchGoogleReviews(t.Context(), " \t", "", 0, 20)
	require.ErrorIs(t, err, googlereviews.ErrSearch)
}

func TestGoogleReviewsSchemaUpgradeAddsArchiveAndSearch(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	for _, table := range []string{"google_reviews_sync_tokens", "google_reviews_sync_state", "google_reviews"} {
		_, err := st.DB().ExecContext(ctx, "DROP TABLE "+table)
		require.NoError(t, err)
	}
	_, err := st.DB().ExecContext(ctx, "UPDATE archive_metadata SET value = '33' WHERE key = 'schema_version'")
	require.NoError(t, err)

	require.NoError(t, st.InitSchemaContext(ctx))
	version, err := st.SchemaVersionContext(ctx)
	require.NoError(t, err)
	assert.Equal(t, store.SchemaVersion, version)

	_, err = st.BeginGoogleReviewsBackfill(ctx, googleReviewTestTarget, googleReviewTestLocation)
	require.NoError(t, err)
	_, err = st.ApplyGoogleReviewsPage(ctx, googleReviewTestTarget, googleReviewTestLocation, "", googlereviews.Page{
		Reviews: []googlereviews.Review{syntheticGoogleReview("r-migrated", "migration search token")},
	})
	require.NoError(t, err)
	response, err := st.SearchGoogleReviews(ctx, "migration", googleReviewTestTarget, 0, 20)
	require.NoError(t, err)
	assert.Equal(t, int64(1), response.Total)
}

var _ googlereviews.Archive = (*store.Store)(nil)
