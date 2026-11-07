package daemonclient

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/businessreviews"
)

func TestBusinessReviewClientDoesNotReplayUnknownConfirmation(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{{502, "business_reviews_unavailable"}, {429, "business_reviews_quota"}, {503, "business_reviews_temporary"}} {
		t.Run(tc.code, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				assert.Equal(t, "/api/v1/business-reviews/control", r.URL.Path)
				assert.Equal(t, "owner", r.Header.Get("X-Api-Key"))
				body, err := io.ReadAll(r.Body)
				if !assert.NoError(t, err) {
					http.Error(w, "fixture request read failed", http.StatusInternalServerError)
					return
				}
				var request businessreviews.Request
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
			_, err = client.ControlBusinessReviews(t.Context(), businessreviews.Request{Action: "confirm_reply", ConfirmationToken: "issued", Confirmed: true})
			require.Error(t, err)
			assert.Equal(t, 1, calls)
		})
	}
}
