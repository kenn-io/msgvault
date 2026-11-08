package daemonclient

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/googlereviews"
)

func TestGoogleReviewClientDoesNotReplayUnknownConfirmation(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{{502, "google_reviews_unavailable"}, {429, "google_reviews_quota"}, {503, "google_reviews_temporary"}} {
		t.Run(tc.code, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				assert.Equal(t, "/api/v1/google-reviews/control", r.URL.Path)
				assert.Equal(t, "owner", r.Header.Get("X-Api-Key"))
				body, err := io.ReadAll(r.Body)
				if !assert.NoError(t, err) {
					http.Error(w, "fixture request read failed", http.StatusInternalServerError)
					return
				}
				var request googlereviews.Request
				if !assert.NoError(t, json.Unmarshal(body, &request)) {
					http.Error(w, "fixture request decode failed", http.StatusInternalServerError)
					return
				}
				assert.Equal(t, "confirm_reply", request.Action)
				w.WriteHeader(tc.status)
				_, err = w.Write([]byte(`{"error":"` + tc.code + `","message":"safe error"}`))
				if !assert.NoError(t, err) {
					return
				}
			}))
			defer server.Close()
			client, err := New(Config{URL: server.URL, APIKey: "owner", AllowInsecure: true})
			require.NoError(t, err)
			defer func() { assert.NoError(t, client.Close()) }()
			_, err = client.ControlGoogleReviews(t.Context(), googlereviews.Request{Action: "confirm_reply", ConfirmationToken: "issued", Confirmed: true})
			require.Error(t, err)
			assert.Equal(t, 1, calls)
		})
	}
}

func TestGoogleReviewsArchiveSearchUsesAuthenticatedGeneratedRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/google-reviews/archive/search", r.URL.Path)
		assert.Equal(t, "synthetic needle", r.URL.Query().Get("q"))
		assert.Equal(t, "shop", r.URL.Query().Get("target"))
		assert.Equal(t, "2", r.URL.Query().Get("page"))
		assert.Equal(t, "10", r.URL.Query().Get("page_size"))
		assert.Equal(t, "owner", r.Header.Get("X-Api-Key"))
		_, err := w.Write([]byte(`{"query":"synthetic needle","target":"shop","total":0,"page":2,"page_size":10,"results":[]}`))
		require.NoError(t, err)
	}))
	defer server.Close()
	client, err := New(Config{URL: server.URL, APIKey: "owner", AllowInsecure: true})
	require.NoError(t, err)
	defer func() { assert.NoError(t, client.Close()) }()
	result, err := client.SearchGoogleReviewsArchive(t.Context(), googlereviews.ArchiveSearchRequest{
		Query: "synthetic needle", Target: "shop", Page: 2, PageSize: 10,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(0), result.Total)
	assert.Equal(t, 2, result.Page)
}
