package youtube

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

func fixtureClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/youtube/v3")
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := newClient(t.Context(), "fixture-client", "fixture-secret", "fixture-refresh", oauth2.Endpoint{TokenURL: srv.URL + "/token"}, srv.URL+"/")
	require.NoError(t, err)
	return c
}

func TestClientRefreshAndSafeErrors(t *testing.T) {
	_, err := NewClient(t.Context(), "", "x", "y")
	require.Error(t, err)
	tokens := 0
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokens++
			w.Header().Set("Content-Type", "application/json")
			assert.NoError(t, r.ParseForm())
			assert.Equal(t, "fixture-refresh", r.Form.Get("refresh_token"))
			_, _ = fmt.Fprint(w, `{"access_token":"fixture-access","expires_in":3600,"token_type":"Bearer"}`)
			return
		}
		assert.Equal(t, "Bearer fixture-access", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"error":{"message":"fixture-secret","errors":[{"reason":"commentsDisabled"},{"reason":"quotaExceeded"}]}}`)
	})
	_, err = c.ownedChannel(t.Context(), "UCfixture", "", 0)
	require.ErrorIs(t, err, ErrQuota)
	assert.NotContains(t, err.Error(), "fixture-secret")
	assert.Equal(t, 1, tokens)
}

func TestExpiredRefreshTokenSuggestsReauthorization(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":"invalid_grant","error_description":"fixture-secret"}`)
	})
	_, err := c.ownedChannel(t.Context(), "UCfixture", "", 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reauthorize")
	assert.NotContains(t, err.Error(), "fixture-secret")
}
