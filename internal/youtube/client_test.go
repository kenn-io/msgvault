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
	for _, tc := range []struct {
		name, response, want string
		status               int
		tokenError           bool
	}{
		{name: "quota takes precedence", status: http.StatusForbidden, response: `{"error":{"message":"fixture-secret","errors":[{"reason":"commentsDisabled"},{"reason":"quotaExceeded"}]}}`, want: "quota"},
		{name: "expired token", status: http.StatusBadRequest, response: `{"error":"invalid_grant","error_description":"fixture-secret"}`, want: "reauthorize", tokenError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			tokens := 0
			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					tokens++
					w.Header().Set("Content-Type", "application/json")
					if tc.tokenError {
						w.WriteHeader(tc.status)
						_, _ = fmt.Fprint(w, tc.response)
						return
					}
					assert.NoError(r.ParseForm())
					assert.Equal("fixture-refresh", r.Form.Get("refresh_token"))
					_, _ = fmt.Fprint(w, `{"access_token":"fixture-access","expires_in":3600,"token_type":"Bearer"}`)
					return
				}
				assert.Equal("Bearer fixture-access", r.Header.Get("Authorization"))
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.response)
			})
			_, err := c.ownedChannel(t.Context(), "UCfixture", "", 0)
			require.Error(err)
			if tc.name == "quota takes precedence" {
				require.ErrorIs(err, ErrQuota)
				assert.Equal(1, tokens)
			}
			assert.Contains(err.Error(), tc.want)
			assert.NotContains(err.Error(), "fixture-secret")
		})
	}
}

func TestClientRequiresCredentials(t *testing.T) {
	_, err := NewClient(t.Context(), "", "x", "y")
	require.Error(t, err)
}
